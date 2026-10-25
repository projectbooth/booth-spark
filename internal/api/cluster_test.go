package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
)

// GET /v1/cluster: the install's admission bounds and how much of them the live runs hold, for
// anyone with a role in the workspace; other workspaces' runs only counted.
func TestV1_Cluster(t *testing.T) {
	s := testStore(t)
	h := NewRouter(Deps{DB: fakeDB{}, Tokens: people, Iframe: callers, SubmitMinRole: auth.RoleEditor, Applications: &Applications{
		Store: s, Admission: runs.Admission{MaxRunning: 10, MaxRunningPerWorkspace: 4, MemoryBudgetMi: 100000},
		Limits:    runs.Limits{MaxExecutors: 2, DefaultExecutors: 2, DriverMemory: "512m", ExecutorMemory: "768m", MaxMemory: "2g", MaxDuration: 6 * time.Hour},
		DriverCPU: runs.CPU{Request: "250m", Limit: "2"}, ExecutorCPU: runs.CPU{Request: "500m", Limit: "1"},
	}})
	get := func(who, ws string) (int, map[string]any) {
		rec := call(h, who, ws, "GET", "/v1/cluster", "")
		var v map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}

	// Nothing live yet.
	code, v := get("viewer", "acme")
	want := `{"capacity":{"liveRuns":0,"liveRunsInWorkspace":0,"maxRunning":10,"maxRunningPerWorkspace":4,"memoryBudgetMi":100000,"memoryInUseInWorkspaceMi":0,"memoryInUseMi":0},` +
		`"driver":{"cpuLimit":"2","cpuRequest":"250m","defaultMemory":"512m"},` +
		`"executor":{"cores":1,"cpuLimit":"1","cpuRequest":"500m","defaultMemory":"768m","maxPerRun":2},"master":"kubernetes"}`
	if got, _ := json.Marshal(v); code != 200 || string(got) != want {
		t.Fatalf("empty: %d\n got %s\nwant %s", code, got, want)
	}

	// Two live runs in acme, one in globex, one ended in acme: only the live ones count.
	sub := func(who, ws, body string) string {
		rec := call(h, who, ws, "POST", "/v1/applications", body)
		if rec.Code != 201 {
			t.Fatalf("%s submit: %d %s", who, rec.Code, rec.Body)
		}
		var r map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return r["id"].(string)
	}
	sub("editor", "acme", pi)
	sub("owner", "acme", pi)
	sub("globex", "globex", pi)
	ended := sub("editor", "acme", pi)
	if err := s.Finish(t.Context(), ended, runs.Succeeded, "", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who, ws              string
		live, here, used, us float64
	}{
		{"viewer", "acme", 3, 2, 3 * 3200, 2 * 3200},
		{"globex", "globex", 3, 1, 3 * 3200, 3200},
	} {
		code, v := get(c.who, c.ws)
		cap, _ := v["capacity"].(map[string]any)
		if code != 200 || cap["liveRuns"] != c.live || cap["liveRunsInWorkspace"] != c.here ||
			cap["memoryInUseMi"] != c.used || cap["memoryInUseInWorkspaceMi"] != c.us {
			t.Errorf("%s in %s: %d %v", c.who, c.ws, code, v)
		}
	}

	// The UI's API serves the same, as the page's viewer.
	if rec := uiCall(h, "viewer", "GET", "/ui/api/cluster", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"liveRunsInWorkspace":2`) {
		t.Errorf("ui/api/cluster: %d %s", rec.Code, rec.Body)
	}

	// Without a role in the workspace: refused, as every /v1 call.
	if code, _ := get("globex", "acme"); code != 403 {
		t.Errorf("a member of another workspace: %d", code)
	}
}

// A run's origin: a person; a pipeline task, its task from the key booth-pipeline sends; any other
// workload by its recorded module and subject kind. "pipeline" needs core's signed booth_module to
// have named the pipeline module: recorded at submit, or, for a run recorded before that, implied
// by a data owner.
func TestOrigin(t *testing.T) {
	pl := func(over runs.Run) runs.Run {
		over.Workload, over.Submitter = true, "pipeline:acme:p8"
		return over
	}
	for _, c := range []struct {
		name string
		run  runs.Run
		want originView
	}{
		{"a person", runs.Run{Submitter: "e-1"}, originView{Kind: "person"}},
		{"a pipeline task", pl(runs.Run{SubmitterModule: "pipeline", DataOwner: "u-alice", IdempotencyKey: "p8/load_games/2"}),
			originView{Kind: "pipeline", Module: "pipeline", PipelineRun: "p8", Task: "load_games", Attempt: "2"}},
		{"a pipeline task without data access", pl(runs.Run{SubmitterModule: "pipeline", IdempotencyKey: "p8/load_games/1"}),
			originView{Kind: "pipeline", Module: "pipeline", PipelineRun: "p8", Task: "load_games", Attempt: "1"}},
		{"a task key with a slash", pl(runs.Run{SubmitterModule: "pipeline", IdempotencyKey: "p8/group/load/1"}),
			originView{Kind: "pipeline", Module: "pipeline", PipelineRun: "p8", Task: "group/load", Attempt: "1"}},
		{"a key for another pipeline run names no task", pl(runs.Run{SubmitterModule: "pipeline", IdempotencyKey: "p9/load/1"}),
			originView{Kind: "pipeline", Module: "pipeline", PipelineRun: "p8"}},
		{"no key", pl(runs.Run{SubmitterModule: "pipeline"}), originView{Kind: "pipeline", Module: "pipeline", PipelineRun: "p8"}},
		{"another module choosing a pipeline-shaped subject", pl(runs.Run{SubmitterModule: "streamlit", IdempotencyKey: "p8/x/1"}),
			originView{Kind: "workload", Module: "streamlit", Workload: "pipeline"}},
		{"recorded before the module was kept, with a data owner", pl(runs.Run{DataOwner: "u-alice", IdempotencyKey: "p8/load/3"}),
			originView{Kind: "pipeline", Module: "pipeline", PipelineRun: "p8", Task: "load", Attempt: "3"}},
		{"recorded before the module was kept, without one", pl(runs.Run{IdempotencyKey: "p8/x/1"}),
			originView{Kind: "workload", Workload: "pipeline"}},
		{"a notebook", runs.Run{Workload: true, Submitter: "notebook:acme:n1", SubmitterModule: "notebooks"},
			originView{Kind: "workload", Module: "notebooks", Workload: "notebook"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := origin(c.run); got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}

	// Through the API: the key a pipeline's Spark task sends (<runId>/<taskKey>/<attempt>).
	s := testStore(t)
	h := dataRouter(t, s)
	rec := call(h, "pipeline", "acme", "POST", "/v1/applications", `{"name":"weekly","main":{"inlinePython":"1"},"dataAccess":{"lakehouse":true}}`,
		"Idempotency-Key", "p8/load_games/1")
	var v struct {
		ID     string     `json:"id"`
		Origin originView `json:"origin"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 201 || v.Origin != (originView{Kind: "pipeline", Module: "pipeline", PipelineRun: "p8", Task: "load_games", Attempt: "1"}) {
		t.Errorf("submit: %d %s", rec.Code, rec.Body)
	}
	g := call(h, "editor", "acme", "GET", "/v1/applications/"+v.ID, "")
	if !strings.Contains(g.Body.String(), `"origin":{"kind":"pipeline","module":"pipeline","pipelineRun":"p8","task":"load_games","attempt":"1"}`) {
		t.Errorf("GET: %s", g.Body)
	}
	if rec := call(h, "editor", "acme", "POST", "/v1/applications", pi); !strings.Contains(rec.Body.String(), `"origin":{"kind":"person"}`) {
		t.Errorf("a person's: %s", rec.Body)
	}
	// Recorded from the token's signed module, not its subject: a pipeline task without data access
	// is the pipeline's; another module choosing a pipeline-shaped subject is that module's.
	for who, want := range map[string]string{
		"pipeline":              `"origin":{"kind":"pipeline","module":"pipeline","pipelineRun":"p8","task":"plain","attempt":"1"}`,
		"streamlit-as-pipeline": `"origin":{"kind":"workload","module":"streamlit","workload":"pipeline"}`,
		"run":                   `"origin":{"kind":"workload","workload":"pipeline"}`,
	} {
		rec := call(h, who, "acme", "POST", "/v1/applications", pi, "Idempotency-Key", "p8/plain/1")
		if rec.Code != 201 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", who, rec.Code, rec.Body)
		}
	}
	if rec := call(h, "editor", "acme", "POST", "/v1/sessions", `{"name":"s"}`); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"origin":{"kind":"person"}`) {
		t.Errorf("a session: %d %s", rec.Code, rec.Body)
	}
}
