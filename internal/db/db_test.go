package db

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Runs against the real PostgreSQL from hack/docker-compose.emulators.yml. Skips without it unless
// BOOTH_TEST_REQUIRE_EMULATORS is set (CI sets it), so a missing database fails CI, never skips green.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_EMULATORS") != "" {
			t.Fatal("BOOTH_TEST_POSTGRES_DSN is unset but BOOTH_TEST_REQUIRE_EMULATORS is set")
		}
		t.Skip("BOOTH_TEST_POSTGRES_DSN unset; see hack/docker-compose.emulators.yml")
	}
	return dsn
}

func TestOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := Open(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var one int
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("select 1 = %d, %v", one, err)
	}
}

// Nothing listening: a real refused connection is retried, then fails with the attempt count.
func TestOpen_Unreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := open(ctx, "postgres://u:p@127.0.0.1:1/x?sslmode=disable&connect_timeout=1", 2*time.Second)
	if err == nil {
		pool.Close()
		t.Fatal("Open succeeded against nothing")
	}
	if !strings.Contains(err.Error(), "still failing after 2s") {
		t.Errorf("err = %v, want it retried to the bound", err)
	}
}

// The server answers but refuses: a wrong password or a missing database fails on the first
// attempt, not after a minute.
func TestOpen_RefusedByTheServerIsNotRetried(t *testing.T) {
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	wrongPassword, missingDB := *u, *u
	wrongPassword.User = url.UserPassword(u.User.Username(), "not-the-password")
	missingDB.Path = "/booth_spark_no_such_database"
	for name, dsn := range map[string]string{"wrong password": wrongPassword.String(), "missing database": missingDB.String()} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		start := time.Now()
		pool, err := open(ctx, dsn, DefaultMaxElapsed)
		cancel()
		if err == nil {
			pool.Close()
			t.Errorf("%s: Open succeeded", name)
			continue
		}
		if !strings.Contains(err.Error(), "(attempt 1; not a connection failure, not retried)") || time.Since(start) > 5*time.Second {
			t.Errorf("%s: err = %v after %s, want one attempt", name, err, time.Since(start))
		}
	}
}
