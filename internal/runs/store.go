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
	// LastActivityAt is a session's last activity (created, or a statement submitted, started or
	// ended); idle shutdown counts from it.
	LastActivityAt *time.Time `json:"lastActivityAt,omitempty"`
	// SessionToken is the bearer the backend presents to a session's runner. Never shown.
	SessionToken string `json:"-"`
	// ContentClearedAt is when the run's content (its log tail; a session's statement code, output
	// and error) was cleared, sessions.resultRetention after it ended.
	ContentClearedAt *time.Time `json:"contentClearedAt,omitempty"`
	// DataBearer is what the run's agents present to fetch its workload token. Never shown.
	DataBearer string `json:"-"`
	// Data is the run's data access as resolved before its launch (nil until then, or with none).
	Data *DataPlan `json:"-"`
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
	stop_requested, created_at, started_at, finished_at, footprint_mi, spec, launched, last_activity_at, session_token,
	content_cleared_at, data_bearer, data`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	var spec, data []byte
	err := row.Scan(&r.ID, &r.Kind, &r.Workspace, &r.Submitter, &r.SubmitterName, &r.Workload, &r.Name, &r.State,
		&r.Reason, &r.Namespace, &r.StopRequested, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.FootprintMi, &spec, &r.Launched,
		&r.LastActivityAt, &r.SessionToken, &r.ContentClearedAt, &r.DataBearer, &data)
	if err != nil {
		return r, err
	}
	if len(data) > 0 && string(data) != "null" {
		r.Data = &DataPlan{}
		if err := json.Unmarshal(data, r.Data); err != nil {
			return r, fmt.Errorf("run %s: data: %w", r.ID, err)
		}
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
		out, err = scanRun(tx.QueryRow(ctx, `INSERT INTO runs (id, kind, workspace, submitter, submitter_name, workload, name, state, namespace, footprint_mi, spec, idempotency_key,
								session_token, last_activity_at, data_bearer)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $9, $10, $11, $12, CASE WHEN $2 = 'session' THEN now() END, $13) RETURNING `+runColumns,
			r.ID, r.Kind, r.Workspace, r.Submitter, r.SubmitterName, r.Workload, r.Name, r.Namespace, r.FootprintMi, spec, key, r.SessionToken,
			r.DataBearer))
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

// List returns a workspace's runs of one kind ("application" or "session"), newest first.
func (s *Store) List(ctx context.Context, workspace, kind string, limit int) ([]Run, error) {
	return s.query(ctx, `SELECT `+runColumns+` FROM runs WHERE workspace = $1 AND kind = $2 ORDER BY created_at DESC, id LIMIT $3`, workspace, kind, limit)
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
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE runs SET state = $2, reason = $3, log_tail = $4, finished_at = $5
			WHERE id = $1 AND state IN ('pending', 'running')`, id, st, truncate(reason, 1000), logTail, at)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		// A session's statements end with it.
		_, err = tx.Exec(ctx, `UPDATE statements SET state = 'cancelled', finished_at = $2,
			error = 'the session ended before this statement finished'
			WHERE run_id = $1 AND state IN ('waiting', 'running')`, id, at)
		return err
	})
}

// RequestStop marks a live run for stopping; the controller does the rest.
func (s *Store) RequestStop(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE runs SET stop_requested = true WHERE id = $1 AND state IN ('pending', 'running')`, id)
	return err
}

// ClearContent clears the content of every run that ended before `before` and isn't cleared yet:
// its log tail and session bearer, and its statements' code, output and error. Their state and
// timestamps stay. It returns how many runs it cleared.
func (s *Store) ClearContent(ctx context.Context, before, at time.Time) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE runs SET log_tail = '', session_token = '', data_bearer = '', content_cleared_at = $2
			WHERE finished_at IS NOT NULL AND finished_at < $1 AND content_cleared_at IS NULL RETURNING id`, before, at)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(ids) == 0 {
			return err
		}
		n = len(ids)
		_, err = tx.Exec(ctx, `UPDATE statements SET code = '', output = NULL, error = '' WHERE run_id = ANY($1)`, ids)
		return err
	})
	return n, err
}

// SetDataPlan records a run's resolved data access (before its launch).
func (s *Store) SetDataPlan(ctx context.Context, id string, p DataPlan) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `UPDATE runs SET data = $2 WHERE id = $1`, id, raw)
	return err
}

// ByDataBearer finds the live run whose agents present bearer. ErrNotFound for none, or for a run
// that has ended: an ended run's bearer opens nothing.
func (s *Store) ByDataBearer(ctx context.Context, bearer string) (Run, error) {
	if bearer == "" {
		return Run{}, ErrNotFound
	}
	r, err := scanRun(s.db.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE data_bearer = $1 AND state IN ('pending', 'running')`, bearer))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
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

// Statement is one statement of a session (docs/design-v0.md item 6).
type Statement struct {
	ID         string          `json:"id"`
	RunID      string          `json:"sessionId"`
	Seq        int             `json:"seq"`
	Kind       string          `json:"kind"`
	Code       string          `json:"code"`
	State      string          `json:"state"`
	Output     json.RawMessage `json:"output,omitempty"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
	StartedAt  *time.Time      `json:"startedAt,omitempty"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
}

// Statement states.
const (
	StatementWaiting   = "waiting"
	StatementRunning   = "running"
	StatementAvailable = "available"
	StatementError     = "error"
	StatementCancelled = "cancelled"
)

// ErrSessionOver is returned for a statement sent to a session that has ended (or is ending).
var ErrSessionOver = errors.New("the session has ended")

// ErrTooManyStatements is returned when a session already has as many waiting or running
// statements as it may queue.
var ErrTooManyStatements = errors.New("too many statements waiting in this session")

const statementColumns = `id, run_id, seq, kind, code, state, output, error, created_at, started_at, finished_at`

func scanStatement(row pgx.Row) (Statement, error) {
	var st Statement
	var out []byte
	err := row.Scan(&st.ID, &st.RunID, &st.Seq, &st.Kind, &st.Code, &st.State, &out, &st.Error, &st.CreatedAt, &st.StartedAt, &st.FinishedAt)
	if len(out) > 0 {
		st.Output = out
	}
	return st, err
}

// AddStatement queues a statement in a live session, at most maxOpen waiting or running at once,
// and counts as activity.
func (s *Store) AddStatement(ctx context.Context, runID, kind, code string, maxOpen int) (Statement, error) {
	var st Statement
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var state string
		var stop bool
		err := tx.QueryRow(ctx, `SELECT state, stop_requested FROM runs WHERE id = $1 AND kind = 'session' FOR UPDATE`, runID).Scan(&state, &stop)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if State(state).Terminal() || stop {
			return ErrSessionOver
		}
		var open, seq int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state IN ('waiting', 'running')), coalesce(max(seq), 0)
			FROM statements WHERE run_id = $1`, runID).Scan(&open, &seq); err != nil {
			return err
		}
		if open >= maxOpen {
			return ErrTooManyStatements
		}
		st, err = scanStatement(tx.QueryRow(ctx, `INSERT INTO statements (id, run_id, seq, kind, code, state)
			VALUES ($1, $2, $3, $4, $5, 'waiting') RETURNING `+statementColumns, "s"+NewID()[1:], runID, seq+1, kind, code))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE runs SET last_activity_at = now() WHERE id = $1`, runID)
		return err
	})
	return st, err
}

// Statements lists a session's statements in order.
func (s *Store) Statements(ctx context.Context, runID string) ([]Statement, error) {
	return s.statements(ctx, `SELECT `+statementColumns+` FROM statements WHERE run_id = $1 ORDER BY seq`, runID)
}

// OpenStatements lists a session's waiting and running statements in order.
func (s *Store) OpenStatements(ctx context.Context, runID string) ([]Statement, error) {
	return s.statements(ctx, `SELECT `+statementColumns+` FROM statements WHERE run_id = $1 AND state IN ('waiting', 'running') ORDER BY seq`, runID)
}

func (s *Store) statements(ctx context.Context, sql string, args ...any) ([]Statement, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Statement{}
	for rows.Next() {
		st, err := scanStatement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// GetStatement returns one statement of a session.
func (s *Store) GetStatement(ctx context.Context, runID, id string) (Statement, error) {
	st, err := scanStatement(s.db.QueryRow(ctx, `SELECT `+statementColumns+` FROM statements WHERE run_id = $1 AND id = $2`, runID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

// StartStatement marks a waiting statement running (it was handed to the runner): activity.
func (s *Store) StartStatement(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.Exec(ctx, `WITH st AS (UPDATE statements SET state = 'running', started_at = $2 WHERE id = $1 AND state = 'waiting' RETURNING run_id)
		UPDATE runs SET last_activity_at = $2 FROM st WHERE runs.id = st.run_id`, id, at)
	return err
}

// FinishStatement records a running statement's result: activity.
func (s *Store) FinishStatement(ctx context.Context, id, state string, output json.RawMessage, errText string, at time.Time) error {
	if state != StatementAvailable && state != StatementError {
		return fmt.Errorf("finish statement: %s is not a result", state)
	}
	var out any
	if len(output) > 0 && string(output) != "null" {
		out = []byte(output)
	}
	_, err := s.db.Exec(ctx, `WITH st AS (UPDATE statements SET state = $2, output = $3, error = $4, finished_at = $5
			WHERE id = $1 AND state = 'running' RETURNING run_id)
		UPDATE runs SET last_activity_at = $5 FROM st WHERE runs.id = st.run_id`, id, state, out, truncate(errText, 64<<10), at)
	return err
}
