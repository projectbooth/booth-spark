package db

// ARCHITECTURE.md item 66 (c): on the homelab a new pod's first connections to PostgreSQL are
// refused for about 3 seconds (k3s's kube-router adding the pod's IP to the NetworkPolicy allow
// sets), and the backend used to exit on the first refusal and restart once. Startup now retries
// connection-level failures for about a minute, as booth-core's internal/dbwait does (copied, not
// imported: core's package is internal), and still fails loudly after that. Anything else (a wrong
// password, a missing database, an SQL error) is not a scheduling race and fails at once.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultMaxElapsed bounds how long startup retries: well past the few seconds item 66 observed,
// short enough that an unreachable server still fails within about a minute.
const DefaultMaxElapsed = 60 * time.Second

const (
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second
)

// The clock, replaced by tests.
var (
	now   = time.Now
	after = time.After
)

// Retry calls fn (one connection attempt) until it succeeds, it fails with an error retryable
// rejects, ctx ends, or maxElapsed has passed since the first attempt. It waits 500ms after the
// first failure, doubling up to 8s, and logs every retry: what, the attempt, its error, the delay.
func Retry(ctx context.Context, what string, maxElapsed time.Duration, retryable func(error) bool, fn func() error) error {
	deadline := now().Add(maxElapsed)
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return fmt.Errorf("%s: %w (attempt %d; not a connection failure, not retried)", what, err, attempt)
		}
		if !now().Before(deadline) {
			return fmt.Errorf("%s: still failing after %s (%d attempts): %w", what, maxElapsed, attempt, err)
		}
		log.Printf("%s: not ready yet (attempt %d: %v); retrying in %s", what, attempt, err, backoff)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: stopped after %d attempts: %w (last error: %v)", what, attempt, ctx.Err(), err)
		case <-after(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// ConnectionFailure reports whether err is the connection never being made: refused, a timeout,
// a DNS failure, or the server still starting up (SQLSTATE 57P03). An error the server answered
// with otherwise (authentication, a missing database, any SQL error) is not one.
func ConnectionFailure(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "57P03" // cannot_connect_now: "the database system is starting up"
	}
	var netErr net.Error // *net.OpError (refused, reset, unreachable), *net.DNSError, timeouts
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) // pgconn's connect_timeout
}
