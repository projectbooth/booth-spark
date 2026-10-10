package db

import (
	"context"
	"os"
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

func TestOpen_Unreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if pool, err := Open(ctx, "postgres://u:p@127.0.0.1:1/x?sslmode=disable&connect_timeout=1"); err == nil {
		pool.Close()
		t.Fatal("Open succeeded against nothing")
	}
}
