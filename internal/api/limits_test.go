package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/identity"
	"github.com/projectbooth/booth-spark/internal/runs"
)

// GET /v1/limits (docs/design-v0.md item 6): what this caller may request, from the same limits the
// UI shows, for anyone with a role in the workspace. booth-pipeline reads a 200 as "installed".
func TestV1_Limits(t *testing.T) {
	l := runs.Limits{MaxExecutors: 2, DefaultExecutors: 1, DriverMemory: "512m", ExecutorMemory: "768m", MaxMemory: "2g",
		MaxDuration: 6 * time.Hour, SessionIdleTimeout: 30 * time.Minute, SessionMaxLifetime: 8 * time.Hour,
		Data: runs.DataLimits{Lakehouse: true, Storage: true}}
	h := NewRouter(Deps{DB: fakeDB{}, Tokens: people, Iframe: callers, SubmitMinRole: auth.RoleEditor,
		Applications: &Applications{Limits: l}})

	for _, c := range []struct {
		who, role string
		canSubmit bool
	}{
		{"editor", "editor", true},
		{"owner", "owner", true},
		{"viewer", "viewer", false},
		{"operator", "viewer", false},
		{"pipeline", "editor", true}, // a workload token: the pipeline's own availability probe
	} {
		rec := call(h, c.who, "acme", "GET", "/v1/limits", "")
		if rec.Code != 200 {
			t.Errorf("%s: %d %s", c.who, rec.Code, rec.Body)
			continue
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"maxExecutors": 2.0, "defaultExecutors": 1.0, "driverMemory": "512m", "executorMemory": "768m", "maxMemory": "2g",
			"maxDuration": "6h0m0s", "sessionIdleTimeout": "30m0s", "sessionMaxLifetime": "8h0m0s",
			"dataAccess": map[string]any{"database": false, "lakehouse": true, "storage": true},
			"role":       c.role, "canSubmit": c.canSubmit, "submitMinRole": "editor", "maxStorageLocations": 4.0,
			"languages": []any{"python"}, "entryPoints": []any{"inlinePython", "python", "jar"},
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s:\n got %s\nwant %s", c.who, gotJSON, wantJSON)
		}
	}

	// The same limits as the UI's.
	var me struct {
		Limits uiLimits `json:"limits"`
	}
	rec := do(h, "GET", "/ui/api/me", identity.HeaderIdentity, "editor")
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || !reflect.DeepEqual(me.Limits, limitsOf(l)) {
		t.Errorf("ui limits: %d %s", rec.Code, rec.Body)
	}

	// With Scala on (ADR 0121): scala among the languages, inlineScala and scala among the entry points.
	l.Scala = true
	hs := NewRouter(Deps{DB: fakeDB{}, Tokens: people, Iframe: callers, SubmitMinRole: auth.RoleEditor, Applications: &Applications{Limits: l}})
	var got struct {
		Languages   []string `json:"languages"`
		EntryPoints []string `json:"entryPoints"`
	}
	_ = json.Unmarshal(call(hs, "editor", "acme", "GET", "/v1/limits", "").Body.Bytes(), &got)
	if strings.Join(got.Languages, ",") != "python,scala" || strings.Join(got.EntryPoints, ",") != "inlinePython,inlineScala,python,scala,jar" {
		t.Errorf("with Scala: %+v", got)
	}
	l.Data.Storage = false
	hs = NewRouter(Deps{DB: fakeDB{}, Tokens: people, Iframe: callers, SubmitMinRole: auth.RoleEditor, Applications: &Applications{Limits: l}})
	_ = json.Unmarshal(call(hs, "editor", "acme", "GET", "/v1/limits", "").Body.Bytes(), &got)
	if strings.Join(got.EntryPoints, ",") != "inlinePython,inlineScala" {
		t.Errorf("with Scala, without booth-storage: %+v", got)
	}

	// Refusals, each for its own reason.
	if rec := call(h, "editor", "other", "GET", "/v1/limits", ""); rec.Code != 403 {
		t.Errorf("no role in the workspace: %d", rec.Code)
	}
	if rec := do(h, "GET", "/v1/limits", auth.HeaderBoothWorkspace, "acme"); rec.Code != 401 {
		t.Errorf("no token: %d", rec.Code)
	}
}
