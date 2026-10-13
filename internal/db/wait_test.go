package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var refused = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}

// fakeClock makes Retry's waits instant and records them; the clock advances by each wait.
func fakeClock(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	clock := time.Unix(0, 0)
	oldNow, oldAfter := now, after
	now = func() time.Time { return clock }
	after = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		clock = clock.Add(d)
		c := make(chan time.Time, 1)
		c <- clock
		return c
	}
	t.Cleanup(func() { now, after = oldNow, oldAfter })
	return &waits
}

func TestRetry_FailsThenSucceeds(t *testing.T) {
	waits := fakeClock(t)
	calls := 0
	err := Retry(context.Background(), "connecting", DefaultMaxElapsed, ConnectionFailure, func() error {
		if calls++; calls <= 3 {
			return refused
		}
		return nil
	})
	if err != nil || calls != 4 {
		t.Fatalf("err = %v after %d calls, want success on the 4th", err, calls)
	}
	if want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}; !reflect.DeepEqual(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

// Never connecting: the waits double to 8s and stop at the bound with the last error and the
// number of attempts. The default bound gives 500ms, 1s, 2s, 4s, then 8s until 60s have passed.
func TestRetry_NeverSucceeds(t *testing.T) {
	waits := fakeClock(t)
	calls := 0
	err := Retry(context.Background(), "connecting to postgres at db:5432", DefaultMaxElapsed, ConnectionFailure, func() error {
		calls++
		return fmt.Errorf("attempt %d: %w", calls, refused)
	})
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	if !reflect.DeepEqual(*waits, want) {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
	if err == nil || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("err = %v, want the last refusal", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "connecting to postgres at db:5432: still failing after 1m0s (12 attempts)") ||
		!strings.Contains(msg, "attempt 12:") {
		t.Errorf("err = %q, want what, the bound, the attempt count and the last error", msg)
	}
}

// A short bound (the override) gives up sooner.
func TestRetry_ShortBound(t *testing.T) {
	waits := fakeClock(t)
	calls := 0
	err := Retry(context.Background(), "connecting", 2*time.Second, ConnectionFailure, func() error { calls++; return refused })
	if err == nil || calls != 4 || len(*waits) != 3 {
		t.Fatalf("err = %v, calls = %d, waits = %v; want failure after 4 attempts and 3 waits (0.5s, 1s, 2s)", err, calls, *waits)
	}
}

func TestRetry_NonRetryableFailsAtOnce(t *testing.T) {
	waits := fakeClock(t)
	for _, e := range []error{
		&pgconn.PgError{Code: "28P01", Message: "password authentication failed"},
		&pgconn.PgError{Code: "3D000", Message: `database "x" does not exist`},
		&pgconn.PgError{Code: "42601", Message: "syntax error"},
		errors.New("cannot parse DSN"),
	} {
		calls := 0
		err := Retry(context.Background(), "connecting", DefaultMaxElapsed, ConnectionFailure, func() error { calls++; return e })
		if calls != 1 || !errors.Is(err, e) || !strings.Contains(err.Error(), "not retried") {
			t.Errorf("%v: %d calls, err = %v; want one attempt and the error", e, calls, err)
		}
	}
	if len(*waits) != 0 {
		t.Errorf("waited %v", *waits)
	}
}

// Cancelled while waiting (SIGTERM during startup): Retry returns at once with the context's error.
func TestRetry_CancelledMidWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	oldAfter := after
	after = func(time.Duration) <-chan time.Time { cancel(); return make(chan time.Time) } // never fires
	t.Cleanup(func() { after = oldAfter })
	calls := 0
	err := Retry(ctx, "connecting", DefaultMaxElapsed, ConnectionFailure, func() error { calls++; return refused })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err = %v after %d calls, want context.Canceled after 1", err, calls)
	}
}

func TestConnectionFailure(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{refused, true},
		{fmt.Errorf("failed to connect: %w", refused), true},
		{&net.DNSError{Err: "no such host", Name: "booth-postgres", IsNotFound: true}, true},
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{fmt.Errorf("timeout: %w", context.DeadlineExceeded), true},
		{&pgconn.PgError{Code: "57P03", Message: "the database system is starting up"}, true},
		{&pgconn.PgError{Code: "28P01"}, false},
		{&pgconn.PgError{Code: "28000"}, false},
		{&pgconn.PgError{Code: "3D000"}, false},
		{&pgconn.PgError{Code: "42P01"}, false},
		{errors.New("something else"), false},
	} {
		if got := ConnectionFailure(c.err); got != c.want {
			t.Errorf("ConnectionFailure(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
