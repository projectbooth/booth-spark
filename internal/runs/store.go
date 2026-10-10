package runs

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// State is a run's lifecycle state (docs/design-v0.md item 6).
type State string

const (
	Pending   State = "pending"
	Running   State = "running"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Stopped   State = "stopped"
)

// Terminal reports whether s is a final state.
func (s State) Terminal() bool { return s == Succeeded || s == Failed || s == Stopped }

// Run is one row of the runs table.
type Run struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Workspace     string     `json:"workspace"`
	Submitter     string     `json:"submitter"`
	SubmitterName string     `json:"submitterName"`
	Workload      bool       `json:"workload"`
	Name          string     `json:"name"`
	State         State      `json:"state"`
	Reason        string     `json:"reason,omitempty"`
	Namespace     string     `json:"-"`
	StopRequested bool       `json:"stopRequested"`
	CreatedAt     time.Time  `json:"createdAt"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	FootprintMi   int        `json:"footprintMi"`
	Spec          Validated  `json:"-"`
	// Launched is set once the namespace and its objects exist.
	Launched bool `json:"-"`
}

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the runs table on the module's own database (ADR 0053).
type Store struct{ db *pgxpool.Pool }

// NewStore wraps a pool.
func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// Migrate applies every migration not yet applied, in name order, each in its own transaction.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("migrations table: %w", err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			// One migrator at a time, so two replicas starting together don't race.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(424242)`); err != nil {
				return err
			}
			var done bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&done); err != nil || done {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

// Admission bounds how many runs may be live at once (docs/design-v0.md item 7).
type Admission struct {
	MaxRunning             int
	MaxRunningPerWorkspace int // 0: no per-workspace cap
	MemoryBudgetMi         int
}

// ErrAtCapacity is a refusal by Admission; the message says which bound was hit.
type ErrAtCapacity struct{ msg string }

func (e ErrAtCapacity) Error() string { return e.msg }

// ErrDuplicate means the idempotency key was already used; Create returns the existing run with it.
var ErrDuplicate = errors.New("idempotency key already used")

// NewID returns a run id: "r" and 11 random characters from [a-z0-9], valid as a label value, a
// DNS label inside "bspark-<id>", and an id uiproxy.ValidID accepts.
func NewID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 11)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "r" + string(b)
}

const runColumns = `id, kind, workspace, submitter, submitter_name, workload, name, state, reason, namespace,
	stop_requested, created_at, started_at, finished_at, footprint_mi, spec, launched`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	var spec []byte
	err := row.Scan(&r.ID, &r.Kind, &r.Workspace, &r.Submitter, &r.SubmitterName, &r.Workload, &r.Name, &r.State,
		&r.Reason, &r.Namespace, &r.StopRequested, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.FootprintMi, &spec, &r.Launched)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(spec, &r.Spec); err != nil {
		return r, fmt.Errorf("run %s: spec: %w", r.ID, err)
	}
	return r, nil
}

// Create admits and records a new pending run. With a non-empty idempotencyKey, a second Create
// by the same submitter in the same workspace with the same key returns the first run and
// ErrDuplicate instead of starting another.
func (s *Store) Create(ctx context.Context, r Run, idempotencyKey string, a Admission) (Run, error) {
	spec, err := json.Marshal(r.Spec)
	if err != nil {
		return r, err
	}
	var out Run
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// Admission is a read-then-insert; one at a time, so two submissions can't both squeeze
		// under the same budget.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(424243)`); err != nil {
			return err
		}
		if idempotencyKey != "" {
			existing, err := scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE workspace = $1 AND submitter = $2 AND idempotency_key = $3`,
				r.Workspace, r.Submitter, idempotencyKey))
			if err == nil {
				out = existing
				return ErrDuplicate
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		var live, liveHere, usedMi int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE workspace = $1), coalesce(sum(footprint_mi), 0)
			FROM runs WHERE state IN ('pending', 'running')`, r.Workspace).Scan(&live, &liveHere, &usedMi); err != nil {
			return err
		}
		switch {
		case live >= a.MaxRunning:
			return ErrAtCapacity{fmt.Sprintf("%d runs are already live on this install (runs.maxRunning); try again when one finishes", live)}
		case a.MaxRunningPerWorkspace > 0 && liveHere >= a.MaxRunningPerWorkspace:
			return ErrAtCapacity{fmt.Sprintf("%d runs are already live in this workspace (runs.maxRunningPerWorkspace)", liveHere)}
		case usedMi+r.FootprintMi > a.MemoryBudgetMi:
			return ErrAtCapacity{fmt.Sprintf("this run needs up to %dMi and %dMi of the %dMi budget is in use (runs.memoryBudget); ask for fewer or smaller executors, or wait", r.FootprintMi, usedMi, a.MemoryBudgetMi)}
		}
		var key *string
		if idempotencyKey != "" {
			key = &idempotencyKey
		}
		var err error
		out, err = scanRun(tx.QueryRow(ctx, `INSERT INTO runs (id, kind, workspace, submitter, submitter_name, workload, name, state, namespace, footprint_mi, spec, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $9, $10, $11) RETURNING `+runColumns,
			r.ID, r.Kind, r.Workspace, r.Submitter, r.SubmitterName, r.Workload, r.Name, r.Namespace, r.FootprintMi, spec, key))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		return err
	})
	return out, err
}

// Get returns one run.
func (s *Store) Get(ctx context.Context, id string) (Run, error) {
	r, err := scanRun(s.db.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// List returns a workspace's runs, newest first.
func (s *Store) List(ctx context.Context, workspace string, limit int) ([]Run, error) {
	return s.query(ctx, `SELECT `+runColumns+` FROM runs WHERE workspace = $1 ORDER BY created_at DESC, id LIMIT $2`, workspace, limit)
}

// Live returns every pending or running run.
func (s *Store) Live(ctx context.Context) ([]Run, error) {
	return s.query(ctx, `SELECT `+runColumns+` FROM runs WHERE state IN ('pending', 'running') ORDER BY created_at`)
}

func (s *Store) query(ctx context.Context, sql string, args ...any) ([]Run, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkLaunched records that a run's namespace and objects exist.
func (s *Store) MarkLaunched(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE runs SET launched = true WHERE id = $1`, id)
	return err
}

// MarkRunning moves a pending run to running.
func (s *Store) MarkRunning(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE runs SET state = 'running', started_at = coalesce(started_at, $2) WHERE id = $1 AND state = 'pending'`, id, at)
	return err
}

// Finish moves a live run to a terminal state with its reason and the tail of its driver log.
// It never overwrites a run that is already terminal.
func (s *Store) Finish(ctx context.Context, id string, st State, reason, logTail string, at time.Time) error {
	if !st.Terminal() {
		return fmt.Errorf("finish: %s is not terminal", st)
	}
	_, err := s.db.Exec(ctx, `UPDATE runs SET state = $2, reason = $3, log_tail = $4, finished_at = $5
		WHERE id = $1 AND state IN ('pending', 'running')`, id, st, truncate(reason, 1000), logTail, at)
	return err
}

// RequestStop marks a live run for stopping; the controller does the rest.
func (s *Store) RequestStop(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE runs SET stop_requested = true WHERE id = $1 AND state IN ('pending', 'running')`, id)
	return err
}

// LogTail returns the driver log kept when the run ended.
func (s *Store) LogTail(ctx context.Context, id string) (string, error) {
	var tail string
	err := s.db.QueryRow(ctx, `SELECT log_tail FROM runs WHERE id = $1`, id).Scan(&tail)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return tail, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
