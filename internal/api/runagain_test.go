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

// GET /v1/applications?limit=: a page of the newest runs after the filters, and whether there are
// more; without limit, as before (at most 200).
func TestV1_ApplicationsLimit(t *testing.T) {
	s := testStore(t)
	h := appsRouter(t, s, auth.RoleEditor, roomy)
	var ids []string
	for i, who := range []string{"editor", "editor2", "editor", "editor2", "editor"} {
		rec := call(h, who, "acme", "POST", "/v1/applications", `{"name":"n`+string(rune('0'+i))+`","main":{"inlinePython":"1"}}`)
		if rec.Code != 201 {
			t.Fatalf("submit: %d %s", rec.Code, rec.Body)
		}
		var v struct{ ID string }
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		ids = append(ids, v.ID)
		time.Sleep(5 * time.Millisecond) // created_at orders them
	}
	list := func(q string) ([]string, bool, int) {
		rec := call(h, "viewer", "acme", "GET", "/v1/applications"+q, "")
		var v struct {
			Applications []struct{ ID string }
			More         bool
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		var got []string
		for _, a := range v.Applications {
			got = append(got, a.ID)
		}
		return got, v.More, rec.Code
	}
	for _, c := range []struct {
		q    string
		want []string
		more bool
	}{
		{"", []string{ids[4], ids[3], ids[2], ids[1], ids[0]}, false},
		{"?limit=2", []string{ids[4], ids[3]}, true},
		{"?limit=5", []string{ids[4], ids[3], ids[2], ids[1], ids[0]}, false},
		// The filters first, then the page.
		{"?limit=2&state=pending", []string{ids[4], ids[3]}, true},
	} {
		got, more, code := list(c.q)
		if code != 200 || strings.Join(got, ",") != strings.Join(c.want, ",") || more != c.more {
			t.Errorf("%q: %d %v more=%v, want %v more=%v", c.q, code, got, more, c.want, c.more)
		}
	}
	rec := call(h, "editor", "acme", "GET", "/v1/applications?mine=true&limit=2", "")
	if !strings.Contains(rec.Body.String(), ids[4]) || !strings.Contains(rec.Body.String(), ids[2]) || strings.Contains(rec.Body.String(), ids[0]) ||
		!strings.Contains(rec.Body.String(), `"more":true`) {
		t.Errorf("mine, limit 2: %s", rec.Body)
	}
	for _, q := range []string{"?limit=0", "?limit=201", "?limit=x"} {
		if _, _, code := list(q); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
}

// GET /v1/applications/{id}/spec: the run's spec as POST takes it, code and arguments included,
// for whoever may read its log; POSTing it starts a new run and leaves the old one as it was. Once
// retention clears the code, codeCleared and no code.
func TestV1_ApplicationSpec(t *testing.T) {
	s := testStore(t)
	h := appsRouter(t, s, auth.RoleEditor, roomy)
	body := `{"name":"weekly","main":{"inlinePython":"print(1)"},"args":["a","b"],"conf":{"spark.sql.shuffle.partitions":"8"},` +
		`"resources":{"driver":{"memory":"1g"},"executors":{"min":1,"max":2,"memory":"768m"}},"maxDuration":"30m"}`
	rec := call(h, "editor", "acme", "POST", "/v1/applications", body)
	var old struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &old)
	if rec.Code != 201 {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body)
	}
	path := "/v1/applications/" + old.ID + "/spec"
	want := `{"spec":{"name":"weekly","main":{"inlinePython":"print(1)"},"args":["a","b"],"resources":{"driver":{"memory":"1g"},` +
		`"executors":{"min":1,"max":2,"memory":"768m"}},"conf":{"spark.sql.shuffle.partitions":"8"},"maxDuration":"30m"},"codeCleared":false}`
	for _, who := range []string{"editor", "owner", "operator"} {
		if rec := call(h, who, "acme", "GET", path, ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: %d\n got %s\nwant %s", who, rec.Code, rec.Body, want)
		}
	}
	// Refused, as the log is: another editor, a viewer; another workspace's run is unknown.
	for who, code := range map[string]int{"editor2": 403, "viewer": 403, "globex": 403} {
		if rec := call(h, who, "acme", "GET", path, ""); rec.Code != code {
			t.Errorf("%s: %d %s", who, rec.Code, rec.Body)
		}
	}
	if rec := call(h, "globex", "globex", "GET", path, ""); rec.Code != 404 {
		t.Errorf("from another workspace: %d", rec.Code)
	}

	// Submitted again: a new run with the same settings; the old one unchanged.
	var sv struct {
		Spec json.RawMessage
	}
	_ = json.Unmarshal(call(h, "editor", "acme", "GET", path, "").Body.Bytes(), &sv)
	rec = call(h, "editor", "acme", "POST", "/v1/applications", string(sv.Spec))
	var again struct {
		ID        string
		Resources struct{ DriverMemory, ExecutorMemory string }
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != 201 || again.ID == old.ID || again.Resources.DriverMemory != "1024m" || again.Resources.ExecutorMemory != "768m" {
		t.Errorf("again: %d %s", rec.Code, rec.Body)
	}
	if got, _ := s.Get(context.Background(), old.ID); got.State != runs.Pending || got.Spec.Main.InlinePython != "print(1)" {
		t.Errorf("the old run changed: %+v", got)
	}

	// Cleared by retention: the settings stay, the code doesn't.
	if err := s.Finish(context.Background(), old.ID, runs.Succeeded, "done", "", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearContent(context.Background(), time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	rec = call(h, "editor", "acme", "GET", path, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"codeCleared":true`) || strings.Contains(rec.Body.String(), "print(1)") ||
		!strings.Contains(rec.Body.String(), `"args":["a","b"]`) || !strings.Contains(rec.Body.String(), `"main":{}`) {
		t.Errorf("cleared: %d %s", rec.Code, rec.Body)
	}
}

// waitingReason: why a pending run still waits, on the run and in the list; gone once it starts.
func TestV1_WaitingReason(t *testing.T) {
	s := testStore(t)
	h := appsRouter(t, s, auth.RoleEditor, roomy)
	rec := call(h, "editor", "acme", "POST", "/v1/applications", pi)
	var v struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if strings.Contains(rec.Body.String(), "waitingReason") {
		t.Errorf("a new run waits for nothing yet: %s", rec.Body)
	}
	why := "resolving data access (will retry): the workspace's warehouse: the credential broker answered 503"
	if err := s.SetWaiting(context.Background(), v.ID, why); err != nil {
		t.Fatal(err)
	}
	want := `"waitingReason":"resolving data access (will retry): the workspace's warehouse: the credential broker answered 503"`
	for _, p := range []string{"/v1/applications/" + v.ID, "/v1/applications"} {
		if rec := call(h, "viewer", "acme", "GET", p, ""); !strings.Contains(rec.Body.String(), want) || !strings.Contains(rec.Body.String(), `"state":"pending"`) {
			t.Errorf("%s: %s", p, rec.Body)
		}
	}
	if err := s.MarkRunning(context.Background(), v.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := call(h, "viewer", "acme", "GET", "/v1/applications/"+v.ID, ""); strings.Contains(rec.Body.String(), "waitingReason") {
		t.Errorf("running: %s", rec.Body)
	}
}
