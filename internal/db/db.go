// Package db opens booth-spark's connection pool against this module's own database on the shared
// PostgreSQL cluster (ADR 0014/0053). Schema and migrations arrive with the first real table (run
// records, step 3); the scaffold only needs a verified connection for /healthz.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open connects to dsn and verifies the connection, riding out a refused first connection for up
// to DefaultMaxElapsed (wait.go).
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	return open(ctx, dsn, DefaultMaxElapsed)
}

func open(ctx context.Context, dsn string, maxElapsed time.Duration) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	what := "connecting to postgres"
	if c := pool.Config().ConnConfig; c != nil {
		what = fmt.Sprintf("connecting to postgres at %s:%d", c.Host, c.Port)
	}
	if err := Retry(ctx, what, maxElapsed, ConnectionFailure, func() error { return pool.Ping(ctx) }); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
