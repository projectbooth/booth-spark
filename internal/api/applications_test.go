package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
)

func testStore(t *testing.T) *runs.Store {
	t.Helper()
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") != "" {
			t.Fatal("BOOTH_TEST_POSTGRES_DSN is unset but BOOTH_TEST_REQUIRE_EMULATORS is set")
		}
		t.Skip("BOOTH_TEST_POSTGRES_DSN unset")
	}
	ctx := context.Background()
	schema := "api_" + runs.NewID()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := runs.NewStore(pool)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

// fixedTokens is a TokenVerifier whose bearer value names the caller's claims.
type fixedTokens map[string]auth.Claims

func (f fixedTokens) Verify(_ context.Context, raw string) (*auth.Claims, error) {
	c, ok := f[raw]
	if !ok {
		return nil, auth.ErrProviderUnavailable
	}
	return &c, nil
}

var people = fixedTokens{
	"editor":   {Subject: "e-1", DisplayName: "ed", Groups: []string{"/workspaces/acme/editor"}},
	"editor2":  {Subject: "e-2", Groups: []string{"/workspaces/acme/editor"}},
	"owner":    {Subject: "o-1", Groups: []string{"/workspaces/acme/owner"}},
	"viewer":   {Subject: "v-1", Groups: []string{"/workspaces/acme/viewer"}},
	"operator": {Subject: "op-1", Groups: []string{"/workspaces/acme/viewer", "/platform/operator"}},
	"globex":   {Subject: "g-1", Groups: []string{"/workspaces/globex/owner"}},
	"run":      {Subject: "pipeline:7", Groups: []string{"/workspaces/acme/editor"}, Workload: true},
}

func appsRouter(t *testing.T, s *runs.Store, minRole auth.Role, adm runs.Admission) http.Handler {
	t.Helper()
	return NewRouter(Deps{DB: fakeDB{}, Tokens: people, Iframe: fakeIframe{}, SubmitMinRole: minRole, Applications: &Applications{
		Store: s, Admission: adm,
		Limits:   runs.Limits{MaxExecutors: 2, DefaultExecutors: 2, DriverMemory: "512m", ExecutorMemory: "512m", MaxMemory: "2g", MaxDuration: 6 * time.Hour},
		LiveLogs: func(_ context.Context, ns string, tail int64) (string, error) { return "live log of " + ns, nil },
	}})
}

func call(h http.Handler, who, ws, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+who)
	req.Header.Set(auth.HeaderBoothWorkspace, ws)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const pi = `{"name":"pi","main":{"inlinePython":"print(1)"}}`

var roomy = runs.Admission{MaxRunning: 10, MemoryBudgetMi: 100000}

func TestApplications_Submit(t *testing.T) {
	s := testStore(t)
	h := appsRouter(t, s, auth.RoleEditor, roomy)

	rec := call(h, "editor", "acme", "POST", "/v1/applications", pi)
	if rec.Code != 201 {
		t.Fatalf("editor submit: %d %s", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["state"] != "pending" || got["submitter"] != "e-1" || got["workspace"] != "acme" || got["footprintMi"] != float64(2688) {
		t.Errorf("run = %v", got)
	}
	if _, leaked := got["Namespace"]; leaked {
		t.Error("the namespace is internal")
	}
	for _, c := range []struct {
		name, who, body string
		status          int
		code            string
	}{
		{"a viewer never submits", "viewer", pi, 403, "forbidden"},
		{"an operator who is a viewer doesn't either", "operator", pi, 403, "forbidden"},
		{"unknown field", "editor", `{"name":"pi","main":{"inlinePython":"x"},"image":"evil"}`, 400, "invalid"},
		{"not JSON", "editor", `nope`, 400, "invalid"},
		{"a file from storage", "editor", `{"name":"pi","main":{"python":{"backendId":"b","path":"p"}}}`, 422, "unavailable"},
		{"data access, as a person", "editor", `{"name":"pi","main":{"inlinePython":"x"},"dataAccess":{"database":true}}`, 422, "unavailable"},
		{"data access, as a workload (ADR 0110 ruling 5)", "run", `{"name":"pi","main":{"inlinePython":"x"},"dataAccess":{"database":true}}`, 422, "unavailable"},
		{"a module setting in conf", "editor", `{"name":"pi","main":{"inlinePython":"x"},"conf":{"spark.kubernetes.namespace":"kube-system"}}`, 400, "invalid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := call(h, c.who, "acme", "POST", "/v1/applications", c.body)
			if rec.Code != c.status || !strings.Contains(rec.Body.String(), `"code":"`+c.code+`"`) {
				t.Errorf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	// A workload caller may submit (without data access) and is recorded as one.
	rec = call(h, "run", "acme", "POST", "/v1/applications", pi)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"workload":true`) {
		t.Errorf("workload submit: %d %s", rec.Code, rec.Body)
	}
	// submit.minRole=owner: editors no longer submit.
	strict := appsRouter(t, s, auth.RoleOwner, roomy)
	if rec := call(strict, "editor", "acme", "POST", "/v1/applications", pi); rec.Code != 403 {
		t.Errorf("editor with owner floor: %d", rec.Code)
	}
	if rec := call(strict, "owner", "acme", "POST", "/v1/applications", pi); rec.Code != 201 {
		t.Errorf("owner with owner floor: %d %s", rec.Code, rec.Body)
	}
}

func TestApplications_AdmissionAndIdempotency(t *testing.T) {
	s := testStore(t)
	h := appsRouter(t, s, auth.RoleEditor, runs.Admission{MaxRunning: 1, MemoryBudgetMi: 100000})
	first := call(h, "editor", "acme", "POST", "/v1/applications", pi, "Idempotency-Key", "k1")
	if first.Code != 201 {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	// The same key again: the same run, 200, no second run (even at capacity).
	again := call(h, "editor", "acme", "POST", "/v1/applications", pi, "Idempotency-Key", "k1")
	var a, b map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &b)
	if again.Code != 200 || a["id"] != b["id"] {
		t.Errorf("retry: %d %v vs %v", again.Code, a["id"], b["id"])
	}
	if rec := call(h, "editor", "acme", "POST", "/v1/applications", pi); rec.Code != 429 || !strings.Contains(rec.Body.String(), "at_capacity") {
		t.Errorf("over capacity: %d %s", rec.Code, rec.Body)
	}
}

func TestApplications_ReadStopLogs(t *testing.T) {
	s := testStore(t)
	h := appsRouter(t, s, auth.RoleEditor, roomy)
	rec := call(h, "editor", "acme", "POST", "/v1/applications", pi)
	var r map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	id := r["id"].(string)

	// Everyone in the workspace sees the run; nobody outside it does.
	for who, want := range map[string]int{"viewer": 200, "editor2": 200, "owner": 200} {
		if rec := call(h, who, "acme", "GET", "/v1/applications/"+id, ""); rec.Code != want {
			t.Errorf("%s get: %d", who, rec.Code)
		}
	}
	if rec := call(h, "globex", "globex", "GET", "/v1/applications/"+id, ""); rec.Code != 404 {
		t.Errorf("another workspace: %d", rec.Code)
	}
	if rec := call(h, "viewer", "acme", "GET", "/v1/applications", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), id) {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "editor2", "acme", "GET", "/v1/applications?mine=true", ""); strings.Contains(rec.Body.String(), id) {
		t.Error("mine=true listed someone else's run")
	}

	// Logs and stop: submitter, workspace owner, operator. Not a viewer, not another editor.
	for who, want := range map[string]int{"viewer": 403, "editor2": 403, "editor": 200, "owner": 200, "operator": 200} {
		if rec := call(h, who, "acme", "GET", "/v1/applications/"+id+"/logs", ""); rec.Code != want {
			t.Errorf("%s logs: %d", who, rec.Code)
		}
	}
	if rec := call(h, "editor", "acme", "GET", "/v1/applications/"+id+"/logs?tail=99999", ""); rec.Code != 400 {
		t.Errorf("tail out of range: %d", rec.Code)
	}
	for _, who := range []string{"viewer", "editor2"} {
		if rec := call(h, who, "acme", "POST", "/v1/applications/"+id+"/stop", ""); rec.Code != 403 {
			t.Errorf("%s stop: %d", who, rec.Code)
		}
	}
	if rec := call(h, "owner", "acme", "POST", "/v1/applications/"+id+"/stop", ""); rec.Code != 202 || !strings.Contains(rec.Body.String(), `"stopRequested":true`) {
		t.Errorf("owner stop: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "editor", "acme", "POST", "/v1/applications/rnope/stop", ""); rec.Code != 404 {
		t.Errorf("unknown: %d", rec.Code)
	}
}

// The Spark UI is looked up from the run records: only while running.
func TestStoreRuns(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	h := appsRouter(t, s, auth.RoleEditor, roomy)
	rec := call(h, "editor", "acme", "POST", "/v1/applications", pi)
	var r map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	id := r["id"].(string)
	lookup := StoreRuns{Store: s}
	if _, ok := lookup.Lookup(ctx, id); ok {
		t.Error("a pending run has a UI")
	}
	_ = s.MarkLaunched(ctx, id)
	_ = s.MarkRunning(ctx, id, time.Now())
	run, ok := lookup.Lookup(ctx, id)
	if !ok || run.Submitter != "e-1" || run.Workspace != "acme" || run.UIURL != "http://driver.bspark-"+id+".svc:4040" {
		t.Errorf("running: %+v %v", run, ok)
	}
	if rec := call(h, "editor", "acme", "GET", "/v1/applications/"+id, ""); !strings.Contains(rec.Body.String(), `"uiPath":"/iframe/spark/runs/`+id+`/ui/"`) {
		t.Errorf("uiPath missing: %s", rec.Body)
	}
}
