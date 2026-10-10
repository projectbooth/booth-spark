package runs

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// endedSession records a session with one finished statement, ended at `ended`.
func endedSession(t *testing.T, s *Store, ended time.Time) (Run, Statement) {
	t.Helper()
	ctx := context.Background()
	r, err := s.Create(ctx, newSession(t, "acme", "u1", time.Minute), "", roomy)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.AddStatement(ctx, r.ID, "sql", "select secret from payroll", 20)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.StartStatement(ctx, st.ID, ended.Add(-time.Minute))
	if err := s.FinishStatement(ctx, st.ID, StatementAvailable, json.RawMessage(`{"type":"table","rows":[["42000"]]}`), "", ended.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, r.ID, Stopped, "idle", "driver log with 42000 in it", ended); err != nil {
		t.Fatal(err)
	}
	return r, st
}

// ADR 0110, step 4 ruling 1: an ended run's content is cleared sessions.resultRetention after it
// ended; its state and timestamps stay.
func TestStore_ClearContent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	retention := 7 * 24 * time.Hour

	old, oldSt := endedSession(t, s, now.Add(-retention-time.Hour))
	recent, recentSt := endedSession(t, s, now.Add(-retention+time.Hour))
	app, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)
	if err := s.Finish(ctx, app.ID, Succeeded, "", "an old application's log", now.Add(-retention-time.Minute)); err != nil {
		t.Fatal(err)
	}
	live, _ := s.Create(ctx, newRun(t, "acme", "u1"), "", roomy)

	n, err := s.ClearContent(ctx, now.Add(-retention), now)
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("cleared %d runs, want at least the old session and the old application", n)
	}

	// Cleared: the old session's statement content, its log tail and runner bearer; the old
	// application's log tail.
	got, _ := s.Get(ctx, old.ID)
	if got.ContentClearedAt == nil || !got.ContentClearedAt.Equal(now) || got.SessionToken != "" {
		t.Errorf("old session: cleared at %v, token %q", got.ContentClearedAt, got.SessionToken)
	}
	if got.State != Stopped || got.Reason != "idle" || got.FinishedAt == nil {
		t.Errorf("old session lost its state or timestamps: %+v", got)
	}
	if tail, _ := s.LogTail(ctx, old.ID); tail != "" {
		t.Errorf("old session's log tail kept: %q", tail)
	}
	st, _ := s.GetStatement(ctx, old.ID, oldSt.ID)
	if st.Code != "" || st.Output != nil || st.Error != "" {
		t.Errorf("old statement's content kept: %+v", st)
	}
	if st.State != StatementAvailable || st.StartedAt == nil || st.FinishedAt == nil || st.Kind != "sql" {
		t.Errorf("old statement lost its state or timestamps: %+v", st)
	}
	if tail, _ := s.LogTail(ctx, app.ID); tail != "" {
		t.Errorf("old application's log tail kept: %q", tail)
	}
	if a, _ := s.Get(ctx, app.ID); a.ContentClearedAt == nil || a.State != Succeeded {
		t.Errorf("old application: %+v", a)
	}

	// Kept: a session ended inside the retention, and a live run.
	got, _ = s.Get(ctx, recent.ID)
	if got.ContentClearedAt != nil || got.SessionToken == "" {
		t.Errorf("recent session cleared: %+v", got)
	}
	if tail, _ := s.LogTail(ctx, recent.ID); tail == "" {
		t.Error("recent session's log tail cleared")
	}
	if st, _ := s.GetStatement(ctx, recent.ID, recentSt.ID); st.Code == "" || st.Output == nil {
		t.Errorf("recent statement cleared: %+v", st)
	}
	if l, _ := s.Get(ctx, live.ID); l.ContentClearedAt != nil {
		t.Errorf("live run cleared: %+v", l)
	}

	// Once is enough: a second pass clears nothing new and doesn't move the timestamp.
	if _, err := s.ClearContent(ctx, now.Add(-retention), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, old.ID); !got.ContentClearedAt.Equal(now) {
		t.Errorf("cleared again: %v", got.ContentClearedAt)
	}
}

func TestController_Expire(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old, oldSt := endedSession(t, s, now.Add(-2*time.Hour))
	recent, _ := endedSession(t, s, now.Add(-30*time.Minute))
	c := &Controller{Store: s, Cluster: &fakeCluster{}, ResultRetention: time.Hour, Now: func() time.Time { return now }}
	c.Expire(ctx)
	if st, _ := s.GetStatement(ctx, old.ID, oldSt.ID); st.Code != "" || st.Output != nil {
		t.Errorf("not cleared after its retention: %+v", st)
	}
	if got, _ := s.Get(ctx, recent.ID); got.ContentClearedAt != nil {
		t.Error("cleared inside its retention")
	}
	// 0 keeps everything.
	c.ResultRetention, c.Now = 0, func() time.Time { return now.Add(time.Hour) }
	c.Expire(ctx)
	if got, _ := s.Get(ctx, recent.ID); got.ContentClearedAt != nil {
		t.Error("cleared with retention 0")
	}
}
