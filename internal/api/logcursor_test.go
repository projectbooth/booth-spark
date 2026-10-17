package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-spark/internal/auth"
	"github.com/projectbooth/booth-spark/internal/runs"
)

// The log cursor (ADR 0119 amendment 3): GET /v1/applications/{id}/logs?from=<offset> returns the
// driver's log from a byte offset and the next offset in X-Log-Offset, the same offsets while the
// run lives and after it ends.
func TestApplications_LogCursor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	live := ""
	var endDuringRead func()
	h := NewRouter(Deps{DB: fakeDB{}, Tokens: people, Iframe: fakeIframe{}, SubmitMinRole: auth.RoleEditor, Applications: &Applications{
		Store: s, Admission: roomy,
		Limits: runs.Limits{MaxExecutors: 2, DefaultExecutors: 2, DriverMemory: "512m", ExecutorMemory: "512m", MaxMemory: "2g", MaxDuration: 6 * time.Hour},
		LiveLogFrom: func(_ context.Context, _ string, from int64, max int) ([]byte, int64, error) {
			if endDuringRead != nil {
				endDuringRead()
				return nil, 0, errors.New("unable to retrieve container logs")
			}
			if from > int64(len(live)) {
				return nil, int64(len(live)), nil
			}
			end := min(from+int64(max), int64(len(live)))
			return []byte(live[from:end]), from, nil
		},
	}})
	get := func(id, query string) *httptest.ResponseRecorder {
		t.Helper()
		return call(h, "editor", "acme", "GET", "/v1/applications/"+id+"/logs"+query, "")
	}
	expect := func(name string, rec *httptest.ResponseRecorder, code int, body, offset, complete, skipped string) {
		t.Helper()
		if rec.Code != code {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, code)
			return
		}
		if code != 200 {
			return
		}
		h := rec.Header()
		if rec.Body.String() != body || h.Get("X-Log-Offset") != offset || h.Get("X-Log-Complete") != complete || h.Get("X-Log-Skipped") != skipped {
			t.Errorf("%s: body %q offset %q complete %q skipped %q; want %q %q %q %q", name, rec.Body, h.Get("X-Log-Offset"),
				h.Get("X-Log-Complete"), h.Get("X-Log-Skipped"), body, offset, complete, skipped)
		}
	}

	id := idOf(t, call(h, "editor", "acme", "POST", "/v1/applications", `{"name":"pi","main":{"inlinePython":"print(1)"}}`).Body.String())

	// Before its launch the log is empty.
	expect("not launched, from 0", get(id, "?from=0"), 200, "", "0", "", "")
	expect("not launched, from 5", get(id, "?from=5"), 400, "", "", "", "")
	for _, q := range []string{"?from=-1", "?from=x", "?from=", "?from=0&tail=10"} {
		expect("bad query "+q, get(id, q), 400, "", "", "", "")
	}

	// Live: each read picks up where the last stopped.
	if err := s.MarkLaunched(ctx, id); err != nil {
		t.Fatal(err)
	}
	live = "line1\nline2\n"
	expect("live, from 0", get(id, "?from=0"), 200, "line1\nline2\n", "12", "", "")
	live += "line3\n"
	expect("live, more", get(id, "?from=12"), 200, "line3\n", "18", "", "")
	expect("live, nothing new", get(id, "?from=18"), 200, "", "18", "", "")
	expect("live, beyond the end", get(id, "?from=100"), 400, "", "", "", "")
	// Others' rules are the plain log's: a viewer reads nothing.
	if rec := call(h, "viewer", "acme", "GET", "/v1/applications/"+id+"/logs?from=0", ""); rec.Code != 403 {
		t.Errorf("a viewer: %d", rec.Code)
	}

	// The run ends while the cursor is being read: what's left is the tail kept at its end, at the
	// same offsets (here the tail kept starts at byte 6 of the whole log).
	endDuringRead = func() {
		if err := s.FinishAt(ctx, id, runs.Succeeded, "the application finished", "line2\nline3\nline4\n", 6, time.Now()); err != nil {
			t.Error(err)
		}
	}
	expect("ended during the read", get(id, "?from=18"), 200, "line4\n", "24", "true", "")
	endDuringRead = nil
	expect("ended, at the end", get(id, "?from=24"), 200, "", "24", "true", "")
	expect("ended, from inside the tail", get(id, "?from=12"), 200, "line3\nline4\n", "24", "true", "")
	expect("ended, from before the tail", get(id, "?from=0"), 200, "line2\nline3\nline4\n", "24", "true", "6")
	expect("ended, beyond the end", get(id, "?from=25"), 400, "", "", "", "")
	// The plain log is unchanged.
	if rec := get(id, ""); rec.Code != 200 || rec.Body.String() != "line2\nline3\nline4\n" || rec.Header().Get("X-Log-Offset") != "" {
		t.Errorf("plain log after the end: %d %q", rec.Code, rec.Body)
	}

	// After retention clears it, 410 as for the plain log.
	if _, err := s.ClearContent(ctx, time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := get(id, "?from=0"); rec.Code != 410 || !strings.Contains(rec.Body.String(), "cleared") {
		t.Errorf("cleared: %d %s", rec.Code, rec.Body)
	}
}
