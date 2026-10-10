package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestToken_FetchWriteRefresh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("renaming over a read-only file is the Linux behaviour run pods rely on; Windows refuses it")
	}
	dir := t.TempDir()
	bearer := filepath.Join(dir, "bearer")
	_ = os.WriteFile(bearer, []byte(strings.Repeat("b", 64)+"\n"), 0o400)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("b", 64) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if n == 2 {
			// A refusal keeps the token already on disk.
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"data_access_refused","reason":"lost access"}`)
			return
		}
		_, _ = io.WriteString(w, `{"token":"tok-`+string('0'+rune(n))+`","expiresAt":"`+time.Now().Add(9*time.Minute).Format(time.RFC3339)+`"}`)
	}))
	defer srv.Close()

	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	c := Config{TokenURL: srv.URL, BearerFile: bearer, TokenFile: filepath.Join(dir, "token"),
		Sleep: func(_ context.Context, d time.Duration) bool {
			waits = append(waits, d)
			if len(waits) == 3 {
				cancel()
				return false
			}
			return true
		}}
	if err := ready(c.TokenFile); err == nil {
		t.Error("ready before any token")
	}
	if err := runToken(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(c.TokenFile)
	if string(got) != "tok-3" {
		t.Errorf("token file = %q, want the latest token", got)
	}
	if err := ready(c.TokenFile); err != nil {
		t.Errorf("not ready with a token: %v", err)
	}
	// Two thirds of 9 minutes, then a 1s retry after the refusal, then two thirds again.
	if waits[0] < 5*time.Minute || waits[0] > 6*time.Minute || waits[1] != time.Second || waits[2] < 5*time.Minute {
		t.Errorf("waits = %v", waits)
	}
	if fi, _ := os.Stat(c.TokenFile); fi.Mode().Perm() != 0o440 {
		t.Errorf("token file mode = %v", fi.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".tmp-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}

	// The test knob caps the refresh.
	c.RefreshMax = 20 * time.Second
	if d := refreshIn(c, token{ExpiresAt: time.Now().Add(10 * time.Minute)}); d != 20*time.Second {
		t.Errorf("refreshIn with RefreshMax = %v", d)
	}
}

func TestWait_HealthAndFetch(t *testing.T) {
	dir := t.TempDir()
	bearer := filepath.Join(dir, "bearer")
	_ = os.WriteFile(bearer, []byte("bearer-value"), 0o400)
	var healthy atomic.Bool
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sidecar.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bearer-value" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
			return
		}
		_, _ = io.WriteString(w, "print('the entry point')\n")
	}))
	defer backend.Close()

	sleeps := 0
	c := Config{BearerFile: bearer, Sleep: func(_ context.Context, _ time.Duration) bool {
		sleeps++
		if sleeps == 3 {
			healthy.Store(true)
		}
		return true
	}}
	to := filepath.Join(dir, "main.py")
	if err := runWait(context.Background(), []string{"--healthz=" + sidecar.URL + "/healthz", "--fetch=" + backend.URL + "/internal/main", "--to=" + to}, c); err != nil {
		t.Fatal(err)
	}
	if sleeps < 3 {
		t.Errorf("didn't wait for the sidecar (%d sleeps)", sleeps)
	}
	if b, _ := os.ReadFile(to); string(b) != "print('the entry point')\n" {
		t.Errorf("entry point = %q", b)
	}
	err := runWait(context.Background(), []string{"--fetch=" + backend.URL + "/missing", "--to=" + filepath.Join(dir, "x.py")}, c)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing entry point: %v", err)
	}
	// A sidecar that never gets a lease: the gate gives up with a reason.
	healthy.Store(false)
	never := Config{Sleep: func(ctx context.Context, _ time.Duration) bool { return false }}
	if err := runWait(context.Background(), []string{"--healthz=" + sidecar.URL}, never); err == nil || !strings.Contains(err.Error(), "no lease") {
		t.Errorf("a sidecar without a lease: %v", err)
	}
}
