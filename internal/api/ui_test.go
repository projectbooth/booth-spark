package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/identity"
	"github.com/projectbooth/booth-spark/internal/runs"
)

var uiCallers = fakeIframe{
	"editor":  {Subject: "u-editor", Workspace: "acme", Role: identity.RoleEditor, DisplayName: "editor-user"},
	"editor2": {Subject: "u-editor2", Workspace: "acme", Role: identity.RoleEditor},
	"viewer":  {Subject: "u-viewer", Workspace: "acme", Role: identity.RoleViewer},
	"owner":   {Subject: "u-owner", Workspace: "acme", Role: identity.RoleOwner},
}

var webFS = fstest.MapFS{
	"index.html":          {Data: []byte(`<!doctype html><script type="module" src="./assets/index-abc.js"></script>`)},
	"assets/index-abc.js": {Data: []byte(`console.log(1)`)},
}

func uiRouter(t *testing.T, s *runs.Store) http.Handler {
	t.Helper()
	return NewRouter(Deps{DB: fakeDB{}, Tokens: people, Iframe: uiCallers, SubmitMinRole: auth.RoleEditor, Web: webFS,
		Applications: &Applications{Store: s, Admission: roomy,
			Limits: runs.Limits{MaxExecutors: 2, DefaultExecutors: 2, DriverMemory: "512m", ExecutorMemory: "512m", MaxMemory: "2g",
				MaxDuration: 6 * time.Hour, SessionIdleTimeout: 20 * time.Minute, SessionMaxLifetime: 12 * time.Hour,
				Data: runs.DataLimits{Database: true}}}})
}

func uiCall(h http.Handler, who, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if who != "" {
		req.Header.Set(identity.HeaderIdentity, who)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The module's UI (step 6): served behind core's assertion, with a strict CSP; its hashed assets
// cached, its page never; an unknown asset is a 404, not the page.
func TestUI_Serving(t *testing.T) {
	h := uiRouter(t, nil)
	if rec := uiCall(h, "", "GET", "/", ""); rec.Code != 401 {
		t.Errorf("the UI without an assertion: %d", rec.Code)
	}
	rec := uiCall(h, "viewer", "GET", "/", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "./assets/index-abc.js") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("index: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'self'", "form-action 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %s", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe") {
		t.Errorf("CSP allows unsafe sources: %s", csp)
	}
	if rec := uiCall(h, "viewer", "GET", "/assets/index-abc.js", ""); rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %d %v", rec.Code, rec.Header())
	}
	if rec := uiCall(h, "viewer", "GET", "/assets/missing.js", ""); rec.Code != 404 {
		t.Errorf("a missing asset: %d", rec.Code)
	}
	if rec := uiCall(h, "viewer", "GET", "/somewhere", ""); rec.Code != 404 {
		t.Errorf("an unknown path: %d (the UI routes by fragment)", rec.Code)
	}
	// Without a build, the placeholder page.
	plain := NewRouter(Deps{DB: fakeDB{}, Iframe: uiCallers, SubmitMinRole: auth.RoleEditor})
	if rec := uiCall(plain, "viewer", "GET", "/", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "<h1>Spark</h1>") {
		t.Errorf("placeholder: %d", rec.Code)
	}
}

// The UI's API is /v1's handlers as the assertion's caller: the same rules, no token.
func TestUI_API(t *testing.T) {
	s := testStore(t)
	h := uiRouter(t, s)
	rec := uiCall(h, "editor", "GET", "/ui/api/me", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"canSubmit":true`) || !strings.Contains(rec.Body.String(), `"maxExecutors":2`) ||
		!strings.Contains(rec.Body.String(), `"dataAccess":{"database":true,"lakehouse":false,"storage":false}`) {
		t.Errorf("me: %d %s", rec.Code, rec.Body)
	}
	if rec := uiCall(h, "viewer", "GET", "/ui/api/me", ""); !strings.Contains(rec.Body.String(), `"canSubmit":false`) {
		t.Errorf("viewer me: %s", rec.Body)
	}
	rec = uiCall(h, "editor", "POST", "/ui/api/applications", `{"name":"pi","main":{"inlinePython":"print(1)"}}`)
	if rec.Code != 201 {
		t.Fatalf("submit through the UI: %d %s", rec.Code, rec.Body)
	}
	id := idOf(t, rec.Body.String())
	got, _ := s.Get(t.Context(), id)
	if got.Submitter != "u-editor" || got.SubmitterName != "editor-user" || got.Workload {
		t.Errorf("the run's submitter = %+v", got)
	}
	if rec := uiCall(h, "viewer", "POST", "/ui/api/applications", `{"name":"pi","main":{"inlinePython":"print(1)"}}`); rec.Code != 403 {
		t.Errorf("a viewer submitting through the UI: %d", rec.Code)
	}
	if rec := uiCall(h, "viewer", "GET", "/ui/api/applications/"+id, ""); rec.Code != 200 {
		t.Errorf("a viewer sees the workspace's run: %d", rec.Code)
	}
	if rec := uiCall(h, "editor2", "POST", "/ui/api/applications/"+id+"/stop", ""); rec.Code != 403 {
		t.Errorf("another editor stopping it through the UI: %d", rec.Code)
	}
	rec = uiCall(h, "editor", "POST", "/ui/api/sessions", `{"name":"explore"}`)
	if rec.Code != 201 {
		t.Fatalf("a session through the UI: %d %s", rec.Code, rec.Body)
	}
	sid := idOf(t, rec.Body.String())
	if rec := uiCall(h, "editor2", "GET", "/ui/api/sessions/"+sid, ""); rec.Code != 404 {
		t.Errorf("another editor sees the session through the UI: %d", rec.Code)
	}
	if rec := uiCall(h, "owner", "POST", "/ui/api/sessions/"+sid+"/statements", `{"kind":"sql","code":"select 1"}`); rec.Code != 403 {
		t.Errorf("an owner typing into it through the UI: %d", rec.Code)
	}
	if rec := uiCall(h, "", "GET", "/ui/api/applications", ""); rec.Code != 401 {
		t.Errorf("the UI's API without an assertion: %d", rec.Code)
	}
	// A bearer token is not the UI's credential.
	req := httptest.NewRequest("GET", "/ui/api/applications", nil)
	req.Header.Set("Authorization", "Bearer editor")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Errorf("a bearer token on the UI's API: %d", rr.Code)
	}
	if rec := uiCall(h, "editor", "GET", "/ui/api/nope", ""); rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Errorf("an unknown UI API path: %d %s", rec.Code, rec.Body)
	}
}
