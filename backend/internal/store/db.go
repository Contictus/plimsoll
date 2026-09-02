// Package store owns database access: the connection pool, the sqlc-generated queries,
// and nothing else. It depends on pgx and the schema; no domain package depends on it in
// reverse.
package store

import (
	"context"
	"fmt"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool opens a connection pool and registers the shopspring/decimal codec on every
// connection, so NUMERIC always arrives as decimal.Decimal and never as a float (L1).
// Registering it here rather than at each call site is deliberate: a caller cannot forget.
// Callers own the returned pool and must Close it.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// The DSN carries a password, so it is never echoed into the error (L13).
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		pgxdecimal.Register(conn.TypeMap())
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return pool, nil
}
