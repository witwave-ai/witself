// Package store is witself-server's Postgres access layer. This first slice
// only opens a connection pool and pings it (to back the readiness probe);
// migrations, the schema, and queries land in later slices.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a Postgres connection pool.
type Store struct {
	pool *pgxpool.Pool
	dsn  string
}

// Open creates a connection pool from a Postgres DSN. The pool connects lazily,
// so Open fails only on a malformed DSN — live connectivity is checked by Ping.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	return &Store{pool: pool, dsn: dsn}, nil
}

// Ping verifies live connectivity to the database, with a short timeout. It is
// wired to /readyz so the server is pulled from the load balancer when the DB
// is unreachable.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.pool.Ping(ctx)
}

// Close releases the pool.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}
