package runs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func newSession(t *testing.T, ws, sub string, idle time.Duration) Run {
	t.Helper()
	l := testLimits()
	l.SessionIdleTimeout, l.SessionMaxLifetime = 20*time.Minute, 12*time.Hour
	v, err := ValidateSession(SessionSpec{Name: "s", IdleTimeout: idle.String()}, l)
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	return Run{ID: id, Kind: "session", Workspace: ws, Submitter: sub, Name: "s", Namespace: NamespacePrefix + id,
		FootprintMi: v.FootprintMi(), Spec: v, SessionToken: "tok-" + id}
}

func TestValidateSession(t *testing.T) {
	l := testLimits()
	l.SessionIdleTimeout, l.SessionMaxLifetime = 20*time.Minute, 12*time.Hour
	v, err := ValidateSession(SessionSpec{Name: "s"}, l)
	if err != nil || v.IdleTimeout != 20*time.Minute || v.Duration != 12*time.Hour {
		t.Fatalf("defaults: %+v %v", v, err)
	}
	if v, err := ValidateSession(SessionSpec{Name: "s", IdleTimeout: "1m", MaxLifetime: "1h"}, l); err != nil || v.IdleTimeout != time.Minute || v.Duration != time.Hour {
		t.Errorf("shorter: %+v %v", v, err)
	}
	for name, s := range map[string]SessionSpec{
		"longer idle":     {Name: "s", IdleTimeout: "21m"},
		"longer lifetime": {Name: "s", MaxLifetime: "13h"},
		"sub-second":      {Name: "s", IdleTimeout: "10ms"},
		"no name":         {},
		"module conf":     {Name: "s", Conf: map[string]string{"spark.kubernetes.namespace": "x"}},
	} {
		if _, err := ValidateSession(s, l); !errors.As(err, new(ValidationError)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ValidateSession(SessionSpec{Name: "s", DataAccess: &DataAccess{Database: true}}, l); !errors.As(err, new(Unavailable)) {
		t.Errorf("data access: %v", err)
	}
}

func TestBuild_Session(t *testing.T) {
	r := newSession(t, "acme", "u1", time.Minute)
	o, err := Build(r, testCluster(), []APIEndpoint{{IP: "1.2.3.4", Port: 6443}})
	if err != nil {
		t.Fatal(err)
	}
	if o.AppConfigMap.Data[mainKey] != SessionRunner || !strings.Contains(SessionRunner, "ThreadingHTTPServer") {
		t.Error("a session's driver doesn't run the session runner")
	}
	if o.SessionSecret == nil || o.SessionSecret.StringData["token"] != r.SessionToken {
		t.Fatalf("session secret = %+v", o.SessionSecret)
	}
	c := o.DriverPod.Spec.Containers[0]
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["BOOTH_SESSION_TOKEN_FILE"] != "/opt/booth/session/token" {
		t.Errorf("env = %v", env)
	}
	found := false
	for _, v := range o.DriverPod.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == SessionSecret && *v.Secret.DefaultMode == 0o440 {
			found = true
		}
	}
	if !found {
		t.Error("the session secret isn't mounted (0440) into the driver")
	}
	// The token goes to the driver only: never into the executor template, never into args or env.
	if strings.Contains(o.AppConfigMap.Data[executorTemplateKey], "session") || strings.Contains(strings.Join(c.Args, " "), r.SessionToken) {
		t.Error("the session token reaches the executors or the command line")
	}
	for _, e := range c.Env {
		if e.Value == r.SessionToken {
			t.Error("the session token is in the driver's environment")
		}
	}
	hasPort := func(ports []int32) bool {
		for _, p := range ports {
			if p == SessionPort {
				return true
			}
		}
		return false
	}
	var svc, pol []int32
	for _, p := range o.DriverService.Spec.Ports {
		svc = append(svc, p.Port)
	}
	for _, np := range o.NetworkPolicies {
		if np.Name == "backend-to-driver" {
			for _, p := range np.Spec.Ingress[0].Ports {
				pol = append(pol, p.Port.IntVal)
			}
		}
	}
	if !hasPort(svc) || !hasPort(pol) {
		t.Errorf("the runner port isn't served (%v) or isn't open to the backend (%v)", svc, pol)
	}
	// Applications don't get any of it.
	app, _ := Build(testRun(t), testCluster(), []APIEndpoint{{IP: "1.2.3.4", Port: 6443}})
	if app.SessionSecret != nil || len(app.DriverService.Spec.Ports) != 3 {
		t.Error("an application got a session's objects")
	}
	r.SessionToken = ""
	if _, err := Build(r, testCluster(), []APIEndpoint{{IP: "1.2.3.4", Port: 6443}}); err == nil {
		t.Error("built a session without a token")
	}
}

func TestStore_Statements(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r, err := s.Create(ctx, newSession(t, "acme", "u1", time.Minute), "", roomy)
	if err != nil {
		t.Fatal(err)
	}
	if r.LastActivityAt == nil || r.SessionToken == "" {
		t.Fatalf("session = %+v", r)
	}
	a, err := s.AddStatement(ctx, r.ID, "sql", "select 1", 2)
	if err != nil || a.Seq != 1 || a.State != StatementWaiting {
		t.Fatalf("first: %+v %v", a, err)
	}
	b, _ := s.AddStatement(ctx, r.ID, "python", "print(2)", 2)
	if _, err := s.AddStatement(ctx, r.ID, "sql", "x", 2); !errors.Is(err, ErrTooManyStatements) {
		t.Errorf("over the cap: %v", err)
	}
	now := time.Now()
	_ = s.StartStatement(ctx, a.ID, now)
	_ = s.FinishStatement(ctx, a.ID, StatementAvailable, json.RawMessage(`{"type":"table"}`), "", now)
	got, _ := s.GetStatement(ctx, r.ID, a.ID)
	if got.State != StatementAvailable || string(got.Output) != `{"type": "table"}` || got.FinishedAt == nil {
		t.Errorf("finished: %+v %s", got, got.Output)
	}
	open, _ := s.OpenStatements(ctx, r.ID)
	if len(open) != 1 || open[0].ID != b.ID {
		t.Errorf("open = %+v", open)
	}
	// Ending the session cancels what's left.
	_ = s.Finish(ctx, r.ID, Stopped, "deleted", "", now)
	got, _ = s.GetStatement(ctx, r.ID, b.ID)
	if got.State != StatementCancelled || !strings.Contains(got.Error, "session ended") {
		t.Errorf("after the session ended: %+v", got)
	}
	if _, err := s.AddStatement(ctx, r.ID, "sql", "x", 2); !errors.Is(err, ErrSessionOver) {
		t.Errorf("into an ended session: %v", err)
	}
	app, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	if _, err := s.AddStatement(ctx, app.ID, "sql", "x", 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("into an application: %v", err)
	}
	if list, _ := s.List(ctx, "acme", "session", 10); len(list) != 1 {
		t.Errorf("sessions listed: %d", len(list))
	}
	if list, _ := s.List(ctx, "acme", "application", 10); len(list) != 1 {
		t.Errorf("applications listed: %d", len(list))
	}
}

// fakeRunner stands in for session runners: statements finish when the test says so.
type fakeRunner struct {
	mu        sync.Mutex
	ready     bool
	submitted []string
	results   map[string]RunnerResult
}

func (f *fakeRunner) RunnerReady(context.Context, string, string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

func (f *fakeRunner) RunnerSubmit(_ context.Context, _ string, _ string, st Statement) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.results[st.ID]; !ok {
		f.results[st.ID] = RunnerResult{State: StatementWaiting}
		f.submitted = append(f.submitted, st.ID)
	}
	return nil
}

func (f *fakeRunner) RunnerResult(_ context.Context, _ string, _ string, id string) (RunnerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.results[id]
	if !ok {
		return r, ErrUnknownStatement
	}
	return r, nil
}

func (f *fakeRunner) finish(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[id] = RunnerResult{State: state, Output: json.RawMessage(`{"type":"text","stdout":"ok"}`), Error: map[string]string{StatementError: "Traceback"}[state]}
}

func TestController_Session(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	runner := &fakeRunner{results: map[string]RunnerResult{}}
	f := &fakeCluster{driver: map[string]DriverState{}, runner: runner}
	newCtrl := func() *Controller {
		return &Controller{Store: s, Cluster: f, PendingTimeout: 10 * time.Minute, LogTailBytes: 1 << 20, Now: func() time.Time { return now }}
	}
	c := newCtrl()
	r, _ := s.Create(ctx, newSession(t, "acme", "u1", time.Minute), "", roomy)
	c.Pass(ctx)
	f.driver[r.Namespace] = DriverState{Exists: true, Phase: corev1.PodRunning}

	// The pod runs, but the runner isn't up yet: still pending.
	c.Pass(ctx)
	if got, _ := s.Get(ctx, r.ID); got.State != Pending {
		t.Fatalf("before the runner answers: %s", got.State)
	}
	runner.ready = true
	c.Pass(ctx)
	if got, _ := s.Get(ctx, r.ID); got.State != Running {
		t.Fatalf("after: %s", got.State)
	}

	// Statements go to the runner one at a time, in order.
	a, _ := s.AddStatement(ctx, r.ID, "sql", "select 1", 20)
	b, _ := s.AddStatement(ctx, r.ID, "python", "raise ValueError", 20)
	c.Pass(ctx)
	if len(runner.submitted) != 1 || runner.submitted[0] != a.ID {
		t.Fatalf("submitted %v", runner.submitted)
	}
	c.Pass(ctx)
	if len(runner.submitted) != 1 {
		t.Fatal("the second statement was handed over while the first still ran")
	}

	// A running statement is activity: no idle stop however long it runs.
	now = now.Add(10 * time.Minute)
	c.Pass(ctx)
	if got, _ := s.Get(ctx, r.ID); got.State != Running {
		t.Fatalf("idle-stopped while a statement ran: %+v", got)
	}

	// A backend restart mid-statement: a new controller re-adopts the session from the database
	// and records the result the runner kept.
	runner.finish(a.ID, StatementAvailable)
	c = newCtrl()
	c.Pass(ctx)
	if got, _ := s.GetStatement(ctx, r.ID, a.ID); got.State != StatementAvailable || !strings.Contains(string(got.Output), "ok") {
		t.Fatalf("after the restart: %+v", got)
	}
	if len(runner.submitted) != 2 || runner.submitted[1] != b.ID {
		t.Fatalf("the next statement wasn't handed over: %v", runner.submitted)
	}

	// A failing statement is recorded as an error; the session goes on.
	runner.finish(b.ID, StatementError)
	c.Pass(ctx)
	if got, _ := s.GetStatement(ctx, r.ID, b.ID); got.State != StatementError || got.Error != "Traceback" {
		t.Errorf("failed statement: %+v", got)
	}
	if got, _ := s.Get(ctx, r.ID); got.State != Running {
		t.Errorf("a failed statement ended the session: %+v", got)
	}

	// A statement recorded as running that the runner never got (a restart between the two
	// writes) is handed over again.
	lost, _ := s.AddStatement(ctx, r.ID, "sql", "select 2", 20)
	_ = s.StartStatement(ctx, lost.ID, now)
	c.Pass(ctx)
	if runner.submitted[len(runner.submitted)-1] != lost.ID {
		t.Errorf("the lost statement wasn't handed over again: %v", runner.submitted)
	}
	runner.finish(lost.ID, StatementAvailable)
	c.Pass(ctx)

	// Idle: nothing waiting or running, and no activity for longer than the idle timeout.
	c.Pass(ctx)
	if got, _ := s.Get(ctx, r.ID); got.State != Running {
		t.Fatalf("stopped before its idle timeout: %+v", got)
	}
	now = now.Add(2 * time.Minute)
	c.Pass(ctx)
	got, _ := s.Get(ctx, r.ID)
	if got.State != Stopped || !strings.Contains(got.Reason, "idle") {
		t.Fatalf("idle: %+v", got)
	}
	if f.deleted[len(f.deleted)-1] != r.Namespace {
		t.Error("its namespace wasn't deleted")
	}
}

func TestController_SessionLifetimeAndDriverExit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	runner := &fakeRunner{results: map[string]RunnerResult{}, ready: true}
	f := &fakeCluster{driver: map[string]DriverState{}, runner: runner}
	c := &Controller{Store: s, Cluster: f, PendingTimeout: 10 * time.Minute, Now: func() time.Time { return now }}

	r := newSession(t, "acme", "u1", 20*time.Minute)
	r.Spec.Duration = time.Hour
	r, _ = s.Create(ctx, r, "", roomy)
	c.Pass(ctx)
	f.driver[r.Namespace] = DriverState{Exists: true, Phase: corev1.PodRunning}
	c.Pass(ctx)
	// Busy the whole time, but past its maximum lifetime.
	_, _ = s.AddStatement(ctx, r.ID, "python", "import time; time.sleep(9999)", 20)
	c.Pass(ctx)
	now = now.Add(61 * time.Minute)
	c.Pass(ctx)
	if got, _ := s.Get(ctx, r.ID); got.State != Stopped || !strings.Contains(got.Reason, "maximum lifetime") {
		t.Errorf("lifetime: %+v", got)
	}

	// A session's driver that exits is a failed session.
	r2, _ := s.Create(ctx, newSession(t, "acme", "u1", time.Minute), "", roomy)
	now = time.Now()
	c.Pass(ctx)
	f.driver[r2.Namespace] = DriverState{Exists: true, Phase: corev1.PodSucceeded}
	c.Pass(ctx)
	if got, _ := s.Get(ctx, r2.ID); got.State != Failed {
		t.Errorf("driver exit: %+v", got)
	}
}
