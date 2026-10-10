package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/identity"
	"github.com/projectbooth/booth-spark/internal/uiproxy"
)

// The route-level rules of docs/design-v0.md item 5 (ADR 0110 ruling 4): only the run's submitter,
// in the run's own workspace, read-only, and never the blocked actions. internal/uiproxy's tests
// cover what reaches the driver.
func TestSparkUI(t *testing.T) {
	var reached []string
	drv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Method+" "+r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path})
	}))
	t.Cleanup(drv.Close)

	callers := fakeIframe{
		"submitter": {Subject: "e-1", Workspace: "acme", Role: identity.RoleEditor},
		"owner":     {Subject: "o-1", Workspace: "acme", Role: identity.RoleOwner},
		"operator":  {Subject: "op-1", Workspace: "acme", Role: identity.RoleViewer, Operator: true},
		// The submitter acting in another workspace: the run is not theirs to see from there.
		"submitter-elsewhere": {Subject: "e-1", Workspace: "globex", Role: identity.RoleOwner},
	}
	h := NewRouter(Deps{DB: fakeDB{}, Iframe: callers, SubmitMinRole: auth.RoleEditor, Runs: StaticRuns{
		"r-1": {ID: "r-1", Workspace: "acme", Submitter: "e-1", UIURL: drv.URL},
	}})

	cases := []struct {
		name, who, method, path string
		status                  int
		reaches                 string
	}{
		{"submitter: the jobs page", "submitter", "GET", "/runs/r-1/ui/jobs/", 200, "GET /jobs/"},
		{"submitter: the REST API", "submitter", "GET", "/runs/r-1/ui/api/v1/applications", 200, "GET /api/v1/applications"},
		{"submitter: HEAD", "submitter", "HEAD", "/runs/r-1/ui/jobs/", 200, "HEAD /jobs/"},
		{"no trailing slash redirects into the prefix", "submitter", "GET", "/runs/r-1/ui", 302, ""},
		{"a workspace owner who isn't the submitter", "owner", "GET", "/runs/r-1/ui/jobs/", 403, ""},
		{"a platform operator", "operator", "GET", "/runs/r-1/ui/jobs/", 403, ""},
		{"the submitter, from another workspace", "submitter-elsewhere", "GET", "/runs/r-1/ui/jobs/", 404, ""},
		{"an unknown run", "submitter", "GET", "/runs/r-2/ui/jobs/", 404, ""},
		{"a reserved id", "submitter", "GET", "/runs/proxy/ui/", 404, ""},
		{"POST (the UI's kill forms)", "submitter", "POST", "/runs/r-1/ui/jobs/job/kill/", 405, ""},
		{"kill by GET", "submitter", "GET", "/runs/r-1/ui/stages/stage/kill/?id=1", 403, ""},
		{"thread dump page", "submitter", "GET", "/runs/r-1/ui/executors/threadDump/?executorId=driver", 403, ""},
		{"heap histogram page", "submitter", "GET", "/runs/r-1/ui/executors/heapHistogram/?executorId=driver", 403, ""},
		{"thread dump REST API", "submitter", "GET", "/runs/r-1/ui/api/v1/applications/local-1/executors/driver/threads", 403, ""},
		{"a dot-dot escape", "submitter", "GET", "/runs/r-1/ui/../../v1/me", 400, ""},
		{"an encoded separator", "submitter", "GET", "/runs/r-1/ui/a%2F..%2Fb", 400, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached = nil
			req := httptest.NewRequest(c.method, c.path, nil)
			req.Header.Set(identity.HeaderIdentity, c.who)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			got := strings.Join(reached, ",")
			if got != c.reaches {
				t.Errorf("the driver saw %q, want %q", got, c.reaches)
			}
		})
	}

	// No assertion: 401 before anything is looked up.
	if rec := do(h, "GET", "/runs/r-1/ui/jobs/"); rec.Code != 401 {
		t.Errorf("no assertion: %d", rec.Code)
	}
	if rec := do(h, "GET", "/runs/r-1/ui", identity.HeaderIdentity, "submitter"); rec.Header().Get("Location") != uiproxy.BasePath("r-1")+"/" {
		t.Errorf("redirect Location = %q", rec.Header().Get("Location"))
	}
}

// With no run table at all, every run is unknown.
func TestSparkUI_NoRuns(t *testing.T) {
	h := NewRouter(Deps{DB: fakeDB{}, Iframe: fakeIframe{"x": {Subject: "e-1", Workspace: "acme", Role: identity.RoleEditor}}, SubmitMinRole: auth.RoleEditor})
	if rec := do(h, "GET", "/runs/r-1/ui/jobs/", identity.HeaderIdentity, "x"); rec.Code != 404 {
		t.Errorf("status %d", rec.Code)
	}
}
