package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
)

func sessionsRouter(t *testing.T, s *runs.Store, minRole auth.Role) (h func(who, ws, method, path, body string) (int, string)) {
	t.Helper()
	r := NewRouter(Deps{DB: fakeDB{}, Tokens: people, Iframe: fakeIframe{}, SubmitMinRole: minRole, Applications: &Applications{
		Store: s, Admission: roomy,
		Limits: runs.Limits{MaxExecutors: 2, DefaultExecutors: 2, DriverMemory: "512m", ExecutorMemory: "512m", MaxMemory: "2g",
			MaxDuration: 6 * time.Hour, SessionIdleTimeout: 20 * time.Minute, SessionMaxLifetime: 12 * time.Hour},
		LiveLogs: func(_ context.Context, ns string, _ int64) (string, error) { return "live " + ns, nil },
	}})
	return func(who, ws, method, path, body string) (int, string) {
		rec := call(r, who, ws, method, path, body)
		return rec.Code, rec.Body.String()
	}
}

func idOf(t *testing.T, body string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("%q: %v", body, err)
	}
	return m["id"].(string)
}

func TestSessions_StartAndSee(t *testing.T) {
	s := testStore(t)
	h := sessionsRouter(t, s, auth.RoleEditor)

	code, body := h("editor", "acme", "POST", "/v1/sessions", `{"name":"explore","idleTimeout":"5m"}`)
	if code != 201 || !strings.Contains(body, `"kind":"session"`) || !strings.Contains(body, `"idleTimeout":"5m0s"`) || !strings.Contains(body, `"maxLifetime":"12h0m0s"`) {
		t.Fatalf("start: %d %s", code, body)
	}
	if strings.Contains(body, "tok") || strings.Contains(body, "essionToken") {
		t.Error("the runner token is shown")
	}
	id := idOf(t, body)
	got, _ := s.Get(context.Background(), id)
	if len(got.SessionToken) != 64 {
		t.Errorf("the session's runner token = %q", got.SessionToken)
	}

	for _, c := range []struct {
		name, who, body string
		status          int
	}{
		{"a viewer never starts one", "viewer", `{"name":"x"}`, 403},
		{"nor an operator who is a viewer", "operator", `{"name":"x"}`, 403},
		{"an idle timeout above the chart's", "editor", `{"name":"x","idleTimeout":"21m"}`, 400},
		{"an unknown field", "editor", `{"name":"x","code":"print(1)"}`, 400},
		{"data access", "editor", `{"name":"x","dataAccess":{"database":true}}`, 422},
		{"data access, as a workload (ADR 0110 ruling 5)", "run", `{"name":"x","dataAccess":{"database":true}}`, 422},
	} {
		if code, body := h(c.who, "acme", "POST", "/v1/sessions", c.body); code != c.status {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	// submit.minRole applies to sessions as to applications.
	strict := sessionsRouter(t, s, auth.RoleOwner)
	if code, _ := strict("editor", "acme", "POST", "/v1/sessions", `{"name":"x"}`); code != 403 {
		t.Errorf("editor with an owner floor: %d", code)
	}

	// Only its submitter and the workspace's owners see it; everyone else gets 404.
	for who, want := range map[string]int{"editor": 200, "owner": 200, "editor2": 404, "viewer": 404, "operator": 404} {
		if code, _ := h(who, "acme", "GET", "/v1/sessions/"+id, ""); code != want {
			t.Errorf("%s get: %d, want %d", who, code, want)
		}
		_, list := h(who, "acme", "GET", "/v1/sessions", "")
		if listed := strings.Contains(list, id); listed != (want == 200) {
			t.Errorf("%s list shows it: %v", who, listed)
		}
		if code, _ := h(who, "acme", "GET", "/v1/sessions/"+id+"/logs", ""); code != want {
			t.Errorf("%s logs: %d", who, code)
		}
	}
	if code, _ := h("globex", "globex", "GET", "/v1/sessions/"+id, ""); code != 404 {
		t.Errorf("another workspace: %d", code)
	}
	// Sessions and applications are separate: neither shows up as the other.
	if code, _ := h("editor", "acme", "GET", "/v1/applications/"+id, ""); code != 404 {
		t.Errorf("a session as an application: %d", code)
	}
	_, apps := h("editor", "acme", "GET", "/v1/applications", "")
	if strings.Contains(apps, id) {
		t.Error("a session is listed as an application")
	}
	_, app := h("editor", "acme", "POST", "/v1/applications", pi)
	if code, _ := h("editor", "acme", "GET", "/v1/sessions/"+idOf(t, app), ""); code != 404 {
		t.Errorf("an application as a session: %d", code)
	}
}

func TestSessions_StatementsAndDelete(t *testing.T) {
	s := testStore(t)
	h := sessionsRouter(t, s, auth.RoleEditor)
	_, body := h("editor", "acme", "POST", "/v1/sessions", `{"name":"explore"}`)
	id := idOf(t, body)

	code, body := h("editor", "acme", "POST", "/v1/sessions/"+id+"/statements", `{"kind":"sql","code":"select 1"}`)
	if code != 201 || !strings.Contains(body, `"state":"waiting"`) || !strings.Contains(body, `"seq":1`) {
		t.Fatalf("statement: %d %s", code, body)
	}
	sid := idOf(t, body)
	for _, c := range []struct {
		name, who, body string
		status          int
	}{
		{"an owner may see the session, not type into it", "owner", `{"kind":"sql","code":"select 1"}`, 403},
		{"another editor doesn't even see it", "editor2", `{"kind":"sql","code":"select 1"}`, 404},
		{"an unknown kind", "editor", `{"kind":"scala","code":"1"}`, 400},
		{"no code", "editor", `{"kind":"sql","code":"  "}`, 400},
		{"an unknown field", "editor", `{"kind":"sql","code":"1","async":true}`, 400},
	} {
		if code, body := h(c.who, "acme", "POST", "/v1/sessions/"+id+"/statements", c.body); code != c.status {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	for who, want := range map[string]int{"editor": 200, "owner": 200, "editor2": 404} {
		if code, _ := h(who, "acme", "GET", "/v1/sessions/"+id+"/statements/"+sid, ""); code != want {
			t.Errorf("%s statement: %d", who, code)
		}
		if code, _ := h(who, "acme", "GET", "/v1/sessions/"+id+"/statements", ""); code != want {
			t.Errorf("%s statements: %d", who, code)
		}
	}
	if code, _ := h("editor", "acme", "GET", "/v1/sessions/"+id+"/statements/snope", ""); code != 404 {
		t.Errorf("unknown statement: %d", code)
	}
	// The queue is bounded.
	for i := 0; i < maxOpenStatements-1; i++ {
		h("editor", "acme", "POST", "/v1/sessions/"+id+"/statements", `{"kind":"python","code":"pass"}`)
	}
	if code, body := h("editor", "acme", "POST", "/v1/sessions/"+id+"/statements", `{"kind":"python","code":"pass"}`); code != 429 {
		t.Errorf("over the queue: %d %s", code, body)
	}

	// Delete: its submitter or an owner, nobody else.
	for _, who := range []string{"editor2", "viewer", "operator"} {
		if code, _ := h(who, "acme", "DELETE", "/v1/sessions/"+id, ""); code != 404 {
			t.Errorf("%s delete: %d", who, code)
		}
	}
	if code, body := h("owner", "acme", "DELETE", "/v1/sessions/"+id, ""); code != 202 || !strings.Contains(body, `"stopRequested":true`) {
		t.Errorf("owner delete: %d %s", code, body)
	}
	// A session being stopped takes no more statements.
	if code, _ := h("editor", "acme", "POST", "/v1/sessions/"+id+"/statements", `{"kind":"sql","code":"select 2"}`); code != 409 && code != 429 {
		t.Errorf("into a session being deleted: %d", code)
	}
	_ = s.Finish(context.Background(), id, runs.Stopped, "deleted on request", "", time.Now())
	if code, _ := h("editor", "acme", "POST", "/v1/sessions/"+id+"/statements", `{"kind":"sql","code":"select 2"}`); code != 409 {
		t.Errorf("into an ended session: %d", code)
	}
	if code, _ := h("editor", "acme", "DELETE", "/v1/sessions/"+id, ""); code != 200 {
		t.Errorf("delete an ended session: %d", code)
	}
}
