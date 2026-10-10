package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
)

func dataRouter(t *testing.T, s *runs.Store) http.Handler {
	t.Helper()
	return NewRouter(Deps{DB: fakeDB{}, Tokens: people, Iframe: fakeIframe{}, SubmitMinRole: auth.RoleEditor, Applications: &Applications{
		Store: s, Admission: roomy,
		Limits: runs.Limits{MaxExecutors: 2, DefaultExecutors: 2, DriverMemory: "512m", ExecutorMemory: "512m", MaxMemory: "2g", MaxDuration: 6 * time.Hour,
			SessionIdleTimeout: 20 * time.Minute, SessionMaxLifetime: 12 * time.Hour,
			Data: runs.DataLimits{Database: true, Lakehouse: true, Storage: true}},
	}})
}

// Data access on /v1 (docs/design-v0.md items 4 and 6): a run or session that asks for it gets a
// data bearer, never shown; it shows what it asked for; a workload token gets none of it (422,
// ADR 0110 ruling 5), an entry point in booth-storage included.
func TestData_Submit(t *testing.T) {
	s := testStore(t)
	h := dataRouter(t, s)
	for path, body := range map[string]string{
		"/v1/applications": `{"name":"etl","main":{"python":{"backendId":"lake","path":"jobs/etl.py"}},"dataAccess":{"database":true,"storage":[{"backendId":"lake","path":"raw"}]}}`,
		"/v1/sessions":     `{"name":"explore","dataAccess":{"lakehouse":true}}`,
	} {
		rec := call(h, "editor", "acme", "POST", path, body)
		if rec.Code != 201 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		var v map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		got, _ := s.Get(context.Background(), v["id"].(string))
		if len(got.DataBearer) != 64 || got.DataBearer == got.SessionToken {
			t.Errorf("%s: data bearer = %q", path, got.DataBearer)
		}
		if strings.Contains(rec.Body.String(), got.DataBearer) || strings.Contains(strings.ToLower(rec.Body.String()), "bearer") {
			t.Errorf("%s: the data bearer is shown: %s", path, rec.Body)
		}
		if _, ok := v["dataAccess"].(map[string]any); !ok {
			t.Errorf("%s: no dataAccess in %s", path, rec.Body)
		}
		g := call(h, "editor", "acme", "GET", strings.Replace(path, "/v1/", "/v1/", 1)+"/"+got.ID, "")
		if strings.Contains(g.Body.String(), got.DataBearer) {
			t.Errorf("GET %s shows the data bearer", path)
		}
	}
	// No data access asked for: no bearer.
	rec := call(h, "editor", "acme", "POST", "/v1/applications", `{"name":"pi","main":{"inlinePython":"print(1)"}}`)
	var v map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if got, _ := s.Get(context.Background(), v["id"].(string)); got.DataBearer != "" || strings.Contains(rec.Body.String(), "dataAccess") {
		t.Errorf("a run without data access: bearer %q, body %s", got.DataBearer, rec.Body)
	}

	for name, body := range map[string]string{
		"a storage entry point": `{"name":"x","main":{"python":{"backendId":"lake","path":"a.py"}}}`,
		"a jar":                 `{"name":"x","main":{"jar":{"backendId":"lake","path":"a.jar"},"mainClass":"a.B"}}`,
		"database":              `{"name":"x","main":{"inlinePython":"1"},"dataAccess":{"database":true}}`,
	} {
		if rec := call(h, "run", "acme", "POST", "/v1/applications", body); rec.Code != 422 {
			t.Errorf("a workload's %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := call(h, "run", "acme", "POST", "/v1/sessions", `{"name":"x","dataAccess":{"storage":[{"backendId":"b","path":"p"}]}}`); rec.Code != 422 {
		t.Errorf("a workload's session with storage: %d", rec.Code)
	}
	if rec := call(h, "editor", "acme", "POST", "/v1/applications", `{"name":"x","main":{"inlinePython":"1"},"dataAccess":{"storage":[{"backendId":"b","path":"../x"}]}}`); rec.Code != 400 {
		t.Errorf("a climbing path: %d", rec.Code)
	}
}
