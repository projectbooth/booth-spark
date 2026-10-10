package runs

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
)

// newTestStore returns a Store on a fresh schema of the test PostgreSQL
// (hack/docker-compose.emulators.yml). Skips without one unless BOOTH_TEST_REQUIRE_EMULATORS is set.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") != "" {
			t.Fatal("BOOTH_TEST_POSTGRES_DSN is unset but BOOTH_TEST_REQUIRE_EMULATORS is set")
		}
		t.Skip("BOOTH_TEST_POSTGRES_DSN unset")
	}
	ctx := context.Background()
	schema := "t_" + strings.ToLower(NewID())
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := NewStore(pool)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Twice: migrations are idempotent.
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func newRun(t *testing.T, ws, sub string) Run {
	t.Helper()
	r := testRun(t)
	r.ID = NewID()
	r.Kind = "application"
	r.Workspace, r.Submitter, r.Name = ws, sub, "pi"
	r.Namespace = NamespacePrefix + r.ID
	r.FootprintMi = r.Spec.FootprintMi()
	return r
}

var roomy = Admission{MaxRunning: 10, MemoryBudgetMi: 100000}

func TestStore_CreateGetList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r, err := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != Pending || r.Launched || r.Spec.MaxExecutors != 2 {
		t.Errorf("created = %+v", r)
	}
	got, err := s.Get(ctx, r.ID)
	if err != nil || got.ID != r.ID || got.Spec.Main.InlinePython != "print(1)" {
		t.Errorf("get = %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, "rnope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	_, _ = s.Create(ctx, newRun(t, "globex", "u2"), "", roomy)
	list, err := s.List(ctx, "acme", "application", 10)
	if err != nil || len(list) != 1 || list[0].Workspace != "acme" {
		t.Errorf("list = %+v, %v", list, err)
	}
}

func TestStore_Admission(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := Admission{MaxRunning: 2, MaxRunningPerWorkspace: 1, MemoryBudgetMi: 4096}
	if _, err := s.Create(ctx, newRun(t, "acme", "u1"), "", a); err != nil {
		t.Fatal(err)
	}
	var capErr ErrAtCapacity
	if _, err := s.Create(ctx, newRun(t, "acme", "u2"), "", a); !errors.As(err, &capErr) || !strings.Contains(err.Error(), "maxRunningPerWorkspace") {
		t.Errorf("per-workspace cap: %v", err)
	}
	// Another workspace fits the count but not the budget (2688Mi + 2688Mi > 4096Mi).
	if _, err := s.Create(ctx, newRun(t, "globex", "u3"), "", a); !errors.As(err, &capErr) || !strings.Contains(err.Error(), "memoryBudget") {
		t.Errorf("budget: %v", err)
	}
	small := newRun(t, "globex", "u3")
	small.FootprintMi = 896
	if _, err := s.Create(ctx, small, "", a); err != nil {
		t.Errorf("a small run that fits: %v", err)
	}
	if _, err := s.Create(ctx, newRun(t, "initech", "u4"), "", Admission{MaxRunning: 2, MemoryBudgetMi: 100000}); !errors.As(err, &capErr) || !strings.Contains(err.Error(), "maxRunning") {
		t.Errorf("install cap: %v", err)
	}
	// A finished run frees its place.
	live, _ := s.Live(ctx)
	if err := s.Finish(ctx, live[0].ID, Succeeded, "done", "log", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, newRun(t, "initech", "u4"), "", Admission{MaxRunning: 2, MemoryBudgetMi: 100000}); err != nil {
		t.Errorf("after one finished: %v", err)
	}
}

func TestStore_Idempotency(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first, err := s.Create(ctx, newRun(t, "acme", "u1"), "key-1", roomy)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Create(ctx, newRun(t, "acme", "u1"), "key-1", roomy)
	if !errors.Is(err, ErrDuplicate) || again.ID != first.ID {
		t.Errorf("same key: %v, %s vs %s", err, again.ID, first.ID)
	}
	// The key is per submitter and workspace.
	if _, err := s.Create(ctx, newRun(t, "acme", "u2"), "key-1", roomy); err != nil {
		t.Errorf("another submitter, same key: %v", err)
	}
}

func TestStore_Lifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	_ = s.MarkLaunched(ctx, r.ID)
	now := time.Now()
	_ = s.MarkRunning(ctx, r.ID, now)
	got, _ := s.Get(ctx, r.ID)
	if got.State != Running || !got.Launched || got.StartedAt == nil {
		t.Errorf("running: %+v", got)
	}
	_ = s.RequestStop(ctx, r.ID)
	if got, _ = s.Get(ctx, r.ID); !got.StopRequested {
		t.Error("stop not recorded")
	}
	if err := s.Finish(ctx, r.ID, Stopped, "stopped", "the tail", now); err != nil {
		t.Fatal(err)
	}
	// Terminal is final.
	_ = s.Finish(ctx, r.ID, Failed, "late", "", now)
	if got, _ = s.Get(ctx, r.ID); got.State != Stopped || got.Reason != "stopped" {
		t.Errorf("finished: %+v", got)
	}
	if tail, _ := s.LogTail(ctx, r.ID); tail != "the tail" {
		t.Errorf("tail = %q", tail)
	}
	if err := s.Finish(ctx, r.ID, Running, "", "", now); err == nil {
		t.Error("Finish accepted a non-terminal state")
	}
}

// fakeCluster records what the controller asks of Kubernetes.
type fakeCluster struct {
	launched, deleted []string
	launchErr         error
	driver            map[string]DriverState
	namespaces        map[string]RunNamespace
	runner            *fakeRunner
}

func (f *fakeCluster) Launch(_ context.Context, r Run) error {
	f.launched = append(f.launched, r.ID)
	return f.launchErr
}
func (f *fakeCluster) Delete(_ context.Context, ns string) error {
	f.deleted = append(f.deleted, ns)
	return nil
}
func (f *fakeCluster) Driver(_ context.Context, ns string) (DriverState, error) {
	return f.driver[ns], nil
}
func (f *fakeCluster) LogTail(_ context.Context, ns string, _ int64) (string, error) {
	return "log of " + ns, nil
}
func (f *fakeCluster) RunNamespaces(context.Context) (map[string]RunNamespace, error) {
	return f.namespaces, nil
}

func TestController(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	f := &fakeCluster{driver: map[string]DriverState{}}
	c := &Controller{Store: s, Cluster: f, PendingTimeout: 10 * time.Minute, LogTailBytes: 1 << 20, Now: func() time.Time { return now }}

	r, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	c.Pass(ctx)
	if got, _ := s.Get(ctx, r.ID); !got.Launched || len(f.launched) != 1 {
		t.Fatalf("not launched: %+v", got)
	}
	f.driver[r.Namespace] = DriverState{Exists: true, Phase: corev1.PodRunning}
	c.Pass(ctx)
	if got, _ := s.Get(ctx, r.ID); got.State != Running {
		t.Errorf("state = %s", got.State)
	}
	f.driver[r.Namespace] = DriverState{Exists: true, Phase: corev1.PodSucceeded}
	c.Pass(ctx)
	got, _ := s.Get(ctx, r.ID)
	tail, _ := s.LogTail(ctx, r.ID)
	if got.State != Succeeded || tail != "log of "+r.Namespace || len(f.deleted) != 1 || f.deleted[0] != r.Namespace {
		t.Errorf("succeeded: %+v tail %q deleted %v", got, tail, f.deleted)
	}

	// A driver that can never start fails the run.
	bad, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	c.Pass(ctx)
	f.driver[bad.Namespace] = DriverState{Exists: true, Phase: corev1.PodPending, Fatal: true, Reason: "ErrImagePull: no"}
	c.Pass(ctx)
	if got, _ := s.Get(ctx, bad.ID); got.State != Failed || !strings.Contains(got.Reason, "ErrImagePull") {
		t.Errorf("fatal: %+v", got)
	}

	// A stop request ends the run and its namespace.
	stop, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	c.Pass(ctx)
	_ = s.RequestStop(ctx, stop.ID)
	c.Pass(ctx)
	if got, _ := s.Get(ctx, stop.ID); got.State != Stopped {
		t.Errorf("stop: %+v", got)
	}

	// A launch failure fails the run and removes whatever was created.
	f.launchErr = errors.New("denied")
	lf, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	c.Pass(ctx)
	if got, _ := s.Get(ctx, lf.ID); got.State != Failed || !strings.Contains(got.Reason, "denied") {
		t.Errorf("launch failure: %+v", got)
	}
	f.launchErr = nil

	// The maximum duration stops a run.
	long, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	c.Pass(ctx)
	f.driver[long.Namespace] = DriverState{Exists: true, Phase: corev1.PodRunning}
	c.Pass(ctx)
	now = now.Add(7 * time.Hour)
	c.Pass(ctx)
	if got, _ := s.Get(ctx, long.ID); got.State != Stopped || !strings.Contains(got.Reason, "maximum duration") {
		t.Errorf("max duration: %+v", got)
	}

	// A pending driver that never starts times out.
	now = time.Now()
	stuck, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	c.Pass(ctx)
	f.driver[stuck.Namespace] = DriverState{Exists: true, Phase: corev1.PodPending, Reason: "not scheduled: 0/1 nodes"}
	now = now.Add(11 * time.Minute)
	c.Pass(ctx)
	if got, _ := s.Get(ctx, stuck.ID); got.State != Failed || !strings.Contains(got.Reason, "not scheduled") {
		t.Errorf("pending timeout: %+v", got)
	}
}

func TestController_Sweep(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	live, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	now := time.Now()
	old := now.Add(-2 * SweepGrace)
	f := &fakeCluster{namespaces: map[string]RunNamespace{
		live.Namespace: {Run: live.ID, Created: old},
		"bspark-rgone": {Run: "rgone", Created: old},
		// Not live either, but just created: spared until it is older than SweepGrace.
		"bspark-rnew": {Run: "rnew", Created: now.Add(-SweepGrace / 2)},
	}}
	c := &Controller{Store: s, Cluster: f, Now: func() time.Time { return now }}
	c.Sweep(ctx)
	if len(f.deleted) != 1 || f.deleted[0] != "bspark-rgone" {
		t.Errorf("swept %v, want only the old orphan", f.deleted)
	}
	delete(f.namespaces, "bspark-rgone") // gone, as the real cluster would have it
	now = now.Add(SweepGrace)
	c.Sweep(ctx)
	if len(f.deleted) != 2 || f.deleted[1] != "bspark-rnew" {
		t.Errorf("swept %v, want the new orphan once it is older than SweepGrace", f.deleted)
	}
}

func (f *fakeCluster) RunnerReady(ctx context.Context, ns, token string) bool {
	return f.runner != nil && f.runner.RunnerReady(ctx, ns, token)
}

func (f *fakeCluster) RunnerSubmit(ctx context.Context, ns, token string, st Statement) error {
	if f.runner == nil {
		return errors.New("no runner")
	}
	return f.runner.RunnerSubmit(ctx, ns, token, st)
}

func (f *fakeCluster) RunnerResult(ctx context.Context, ns, token, id string) (RunnerResult, error) {
	if f.runner == nil {
		return RunnerResult{}, errors.New("no runner")
	}
	return f.runner.RunnerResult(ctx, ns, token, id)
}
