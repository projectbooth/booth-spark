package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/auth/authtest"
	"github.com/projectbooth/booth-spark/internal/identity"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

// fakeIframe stands in for identity.Verifier: the assertion's value names the caller.
type fakeIframe map[string]identity.Caller

func (f fakeIframe) Verify(r *http.Request) (identity.Caller, error) {
	raw := r.Header.Get(identity.HeaderIdentity)
	if raw == "" {
		return identity.Caller{}, identity.ErrMissing
	}
	if raw == "forbidden" {
		return identity.Caller{}, identity.ErrForbidden
	}
	c, ok := f[raw]
	if !ok {
		return identity.Caller{}, identity.ErrInvalid
	}
	return c, nil
}

var callers = fakeIframe{
	"editor": {Subject: "e-1", DisplayName: "ed", Workspace: "acme", Role: identity.RoleEditor},
	"viewer": {Subject: "v-1", DisplayName: "<script>alert(1)</script>", Workspace: "acme", Role: identity.RoleViewer, Operator: true},
}

func router(t *testing.T, tokens auth.TokenVerifier, db Pinger, minRole auth.Role) http.Handler {
	t.Helper()
	return NewRouter(Deps{DB: db, Tokens: tokens, Iframe: callers, SubmitMinRole: minRole})
}

func do(h http.Handler, method, path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealth(t *testing.T) {
	ok := router(t, nil, fakeDB{}, auth.RoleEditor)
	if rec := do(ok, "GET", "/healthz"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"database":"ok"`) {
		t.Errorf("healthy: %d %s", rec.Code, rec.Body)
	}
	down := router(t, nil, fakeDB{err: errors.New("down")}, auth.RoleEditor)
	if rec := do(down, "GET", "/healthz"); rec.Code != 503 || !strings.Contains(rec.Body.String(), `"database":"unreachable"`) {
		t.Errorf("db down: %d %s", rec.Code, rec.Body)
	}
	// Liveness ignores the database: a restart can't fix it.
	if rec := do(down, "GET", "/livez"); rec.Code != 200 {
		t.Errorf("livez with db down: %d", rec.Code)
	}
	if rec := do(router(t, nil, nil, auth.RoleEditor), "GET", "/healthz"); rec.Code != 503 {
		t.Errorf("no db: %d", rec.Code)
	}
}

func decodeMe(t *testing.T, rec *httptest.ResponseRecorder) me {
	t.Helper()
	var m me
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	return m
}

func TestV1_Me(t *testing.T) {
	idp := authtest.New(t)
	core := authtest.NewCore(t)
	v, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-design", WorkloadIssuerURL: core.URL})
	if err != nil {
		t.Fatal(err)
	}
	editor := idp.Mint(t, authtest.Token{Subject: "e-1", Groups: []string{"/workspaces/acme/editor"}, Extra: map[string]any{"preferred_username": "ed"}})
	viewer := idp.Mint(t, authtest.Token{Subject: "v-1", Groups: []string{"/workspaces/acme/viewer"}})
	owner := idp.Mint(t, authtest.Token{Subject: "o-1", Groups: []string{"/workspaces/acme/owner"}})
	run := core.Mint(t, authtest.Token{Subject: "pipeline:9", Groups: []string{"/workspaces/acme/editor"}})

	cases := []struct {
		name, token, minRole string
		status               int
		role                 string
		canSubmit, workload  bool
	}{
		{"editor, editor floor", editor, "editor", 200, "editor", true, false},
		{"viewer never submits", viewer, "editor", 200, "viewer", false, false},
		{"editor, owner floor", editor, "owner", 200, "editor", false, false},
		{"owner, owner floor", owner, "owner", 200, "owner", true, false},
		{"workload token", run, "editor", 200, "editor", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := router(t, v, fakeDB{}, auth.Role(c.minRole))
			rec := do(h, "GET", "/v1/me", "Authorization", "Bearer "+c.token, auth.HeaderBoothWorkspace, "acme")
			if rec.Code != c.status {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			m := decodeMe(t, rec)
			if m.Role != c.role || m.CanSubmit != c.canSubmit || m.Workload != c.workload || m.Workspace != "acme" || m.SubmitMinRole != c.minRole {
				t.Errorf("me = %+v", m)
			}
		})
	}

	h := router(t, v, fakeDB{}, auth.RoleEditor)
	// ADR 0041, on a request that bypassed the gateway: a forged role header is refused.
	if rec := do(h, "GET", "/v1/me", "Authorization", "Bearer "+editor, auth.HeaderBoothWorkspace, "acme", auth.HeaderBoothRole, "owner"); rec.Code != 403 {
		t.Errorf("forged role header: %d", rec.Code)
	}
	if rec := do(h, "GET", "/v1/me"); rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
	// The iframe assertion is not a /v1 credential.
	if rec := do(h, "GET", "/v1/me", identity.HeaderIdentity, "editor", auth.HeaderBoothWorkspace, "acme"); rec.Code != 401 {
		t.Errorf("assertion on /v1: %d", rec.Code)
	}
	if rec := do(h, "GET", "/v1/nope", "Authorization", "Bearer "+editor, auth.HeaderBoothWorkspace, "acme"); rec.Code != 404 {
		t.Errorf("unknown /v1 path: %d", rec.Code)
	}
	if rec := do(h, "GET", "/v1/nope"); rec.Code != 401 {
		t.Errorf("unknown /v1 path without a token: %d, want 401 before routing", rec.Code)
	}
}

func TestV1_NotConfigured(t *testing.T) {
	h := router(t, nil, fakeDB{}, auth.RoleEditor)
	if rec := do(h, "GET", "/v1/me", "Authorization", "Bearer x"); rec.Code != 503 {
		t.Errorf("no issuer configured: %d", rec.Code)
	}
}

func TestIframeRoutes(t *testing.T) {
	h := router(t, nil, fakeDB{}, auth.RoleEditor)
	for _, path := range []string{"/", "/ui/api/me", "/anything"} {
		if rec := do(h, "GET", path); rec.Code != 401 {
			t.Errorf("%s without an assertion: %d", path, rec.Code)
		}
	}
	if rec := do(h, "GET", "/", identity.HeaderIdentity, "forbidden"); rec.Code != 403 {
		t.Errorf("assertion granting nothing: %d", rec.Code)
	}
	// A bearer token is not an iframe credential.
	if rec := do(h, "GET", "/ui/api/me", "Authorization", "Bearer editor"); rec.Code != 401 {
		t.Errorf("bearer on an iframe route: %d", rec.Code)
	}

	rec := do(h, "GET", "/ui/api/me", identity.HeaderIdentity, "editor")
	if rec.Code != 200 {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}
	if m := decodeMe(t, rec); m.Subject != "e-1" || m.Role != "editor" || !m.CanSubmit || m.Operator {
		t.Errorf("editor me = %+v", m)
	}
	if m := decodeMe(t, do(h, "GET", "/ui/api/me", identity.HeaderIdentity, "viewer")); m.CanSubmit || !m.Operator {
		t.Errorf("viewer me = %+v", m)
	}

	page := do(h, "GET", "/", identity.HeaderIdentity, "viewer")
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "<title>Spark</title>") {
		t.Fatalf("page: %d %s", page.Code, body)
	}
	if strings.Contains(body, "<script>alert(1)</script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the display name was not escaped")
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	if rec := do(h, "GET", "/anything", identity.HeaderIdentity, "editor"); rec.Code != 404 {
		t.Errorf("unknown iframe path: %d", rec.Code)
	}
}

// Core's iframe entry URL carries its navigation token in the query string: the access log must
// never write it.
func TestAccessLog_NoQueryString(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	h := router(t, nil, fakeDB{}, auth.RoleEditor)
	do(h, "GET", "/?booth_iframe_token=secret-navigation-token", identity.HeaderIdentity, "editor")
	out := buf.String()
	if strings.Contains(out, "secret-navigation-token") || !strings.Contains(out, "http: GET / 200") {
		t.Errorf("log = %q", out)
	}
}

func TestCanSubmit(t *testing.T) {
	for _, c := range []struct {
		role, min auth.Role
		want      bool
	}{
		{auth.RoleViewer, auth.RoleEditor, false},
		{auth.RoleEditor, auth.RoleEditor, true},
		{auth.RoleOwner, auth.RoleEditor, true},
		{auth.RoleEditor, auth.RoleOwner, false},
		{auth.RoleOwner, auth.RoleOwner, true},
		// A misconfigured floor never admits a viewer.
		{auth.RoleViewer, auth.RoleViewer, false},
		{"", auth.RoleEditor, false},
	} {
		if got := canSubmit(c.role, c.min); got != c.want {
			t.Errorf("canSubmit(%q, %q) = %v", c.role, c.min, got)
		}
	}
}
