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
// data bearer, never shown; it shows what it asked for; a workload token without a pipeline's
// signed owner gets none of it (422, ADR 0110 ruling 5 as ADR 0119 leaves it), an entry point in
// booth-storage included.
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

// ADR 0119 (amendment 2): a run a pipeline submits reads and writes as the pipeline's owner, named
// only by core's signed booth_owner claim.
func TestData_PipelineSubmit(t *testing.T) {
	s := testStore(t)
	h := dataRouter(t, s)
	ctx := context.Background()
	const write = `"dataAccess":{"storage":[{"backendId":"spark","path":"smoke","access":"readwrite"}]}`
	recorded := func(t *testing.T, body []byte) runs.Run {
		t.Helper()
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		id, _ := v["id"].(string)
		got, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("no run recorded: %v (%s)", err, body)
		}
		return got
	}

	// 1. An owner in the body is refused (DisallowUnknownFields), wherever it is put, and nothing
	// is recorded.
	for name, body := range map[string]string{
		"top level":      `{"name":"x","main":{"inlinePython":"1"},"owner":"u-mallory",` + write + `}`,
		"in dataAccess":  `{"name":"x","main":{"inlinePython":"1"},"dataAccess":{"owner":"u-mallory","storage":[{"backendId":"spark","path":"smoke","access":"readwrite"}]}}`,
		"as booth_owner": `{"name":"x","main":{"inlinePython":"1"},"booth_owner":"u-mallory",` + write + `}`,
	} {
		if rec := call(h, "pipeline", "acme", "POST", "/v1/applications", body); rec.Code != 400 {
			t.Errorf("a body owner %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if list, _ := s.List(ctx, "acme", "application", 50); len(list) != 0 {
		t.Fatalf("a refused submission was recorded: %+v", list)
	}

	// 2. A pipeline token without the claim: no data access, and no entry point from booth-storage;
	// without data it runs as before.
	for name, body := range map[string]string{
		"storage":               `{"name":"x","main":{"inlinePython":"1"},` + write + `}`,
		"a storage entry point": `{"name":"x","main":{"python":{"backendId":"spark","path":"a.py"}}}`,
	} {
		rec := call(h, "run", "acme", "POST", "/v1/applications", body)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), "booth_owner") {
			t.Errorf("no claim, %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := call(h, "run", "acme", "POST", "/v1/applications", `{"name":"pi","main":{"inlinePython":"1"}}`); rec.Code != 201 {
		t.Errorf("no claim, no data: %d %s", rec.Code, rec.Body)
	}

	// The subject kind is part of the rule (amendment 2, ruling 1): a run's own token, or another
	// module's, carrying a claim gets nothing.
	for _, who := range []string{"spark-run", "streamlit-app"} {
		rec := call(h, who, "acme", "POST", "/v1/applications", `{"name":"x","main":{"inlinePython":"1"},`+write+`}`)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), "only when a pipeline submits it") {
			t.Errorf("%s with a claim: %d %s", who, rec.Code, rec.Body)
		}
	}

	// 3. A pipeline holding only a viewer's role can't submit at all, so it never gets as far as a
	// write (a viewer-capped token's refusal is internal/dataaccess's test). The ceiling is the
	// lesser of editor and the token's role.
	if rec := call(h, "pipeline-viewer", "acme", "POST", "/v1/applications", `{"name":"x","main":{"inlinePython":"1"},`+write+`}`); rec.Code != 403 {
		t.Errorf("a viewer pipeline: %d %s", rec.Code, rec.Body)
	}
	for role, want := range map[auth.Role]string{auth.RoleViewer: "viewer", auth.RoleEditor: "editor", auth.RoleOwner: "editor"} {
		if got := dataRoleCeiling(role); got != want {
			t.Errorf("ceiling for a %s token = %s, want %s", role, got, want)
		}
	}

	// 5. The positive control: recorded as the pipeline's run, reading as its owner, capped at
	// editor, fixed at submit; shown with its owner and, even running, no Spark UI link.
	rec := call(h, "pipeline", "acme", "POST", "/v1/applications", `{"name":"weekly","main":{"inlinePython":"1"},`+write+`}`)
	if rec.Code != 201 {
		t.Fatalf("pipeline with a claim: %d %s", rec.Code, rec.Body)
	}
	got := recorded(t, rec.Body.Bytes())
	if got.Submitter != "pipeline:8" || !got.Workload || got.DataOwner != "u-alice" || got.DataCeiling != "editor" ||
		got.DataOwnerID() != "u-alice" || len(got.DataBearer) != 64 {
		t.Errorf("recorded %+v", got)
	}
	if err := s.MarkRunning(ctx, got.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	g := call(h, "pipeline", "acme", "GET", "/v1/applications/"+got.ID, "")
	var v map[string]any
	_ = json.Unmarshal(g.Body.Bytes(), &v)
	if v["dataOwner"] != "u-alice" || v["dataCeiling"] != "editor" || v["workload"] != true || v["state"] != "running" {
		t.Errorf("GET shows %s", g.Body)
	}
	if _, ok := v["uiPath"]; ok {
		t.Errorf("a pipeline's running run has a Spark UI link: %s", g.Body)
	}

	// A service-account owner is opaque, and an entry point in booth-storage is allowed.
	rec = call(h, "pipeline-svc", "acme", "POST", "/v1/applications", `{"name":"backfill","main":{"python":{"backendId":"spark","path":"jobs/b.py"}}}`)
	if rec.Code != 201 {
		t.Fatalf("service-account owner with a storage entry point: %d %s", rec.Code, rec.Body)
	}
	if got := recorded(t, rec.Body.Bytes()); got.DataOwner != "svc:acme:nightly" || got.DataCeiling != "editor" {
		t.Errorf("recorded %+v", got)
	}

	// A person's run is unchanged: no owner or ceiling recorded, and its Spark UI link stays.
	rec = call(h, "editor", "acme", "POST", "/v1/applications", `{"name":"mine","main":{"inlinePython":"1"},`+write+`}`)
	mine := recorded(t, rec.Body.Bytes())
	if mine.DataOwner != "" || mine.DataCeiling != "" || mine.DataOwnerID() != "e-1" || mine.DataRoleCeiling() != "editor" {
		t.Errorf("a person's run: %+v", mine)
	}
	if err := s.MarkRunning(ctx, mine.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if g := call(h, "editor", "acme", "GET", "/v1/applications/"+mine.ID, ""); !strings.Contains(g.Body.String(), `"uiPath"`) || strings.Contains(g.Body.String(), "dataOwner") {
		t.Errorf("a person's running run: %s", g.Body)
	}

	// Sessions are outside ADR 0119: a pipeline's session gets no data access, claim or not.
	if rec := call(h, "pipeline", "acme", "POST", "/v1/sessions", `{"name":"x","dataAccess":{"lakehouse":true}}`); rec.Code != 422 {
		t.Errorf("a pipeline's session with data: %d %s", rec.Code, rec.Body)
	}
}
