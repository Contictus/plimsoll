// Package tenancy is the only path to tenant-scoped data. It opens a transaction, binds
// the account to it, and hands the caller a store.Queries bound to that transaction.
// No other package may take a connection from the pool for tenant data (K15, L12).
package tenancy

import (
	"context"
	"errors"
	"fmt"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Beginner is the slice of a connection pool this package needs. Taking an interface
// rather than *pgxpool.Pool means a caller -- an HTTP handler, a service -- never has to
// import pgxpool, which is what lets the depguard rule forbid a pool in a domain package
// without also forbidding tenancy itself.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ErrNoAccount is returned when a transaction is requested for the nil account. Binding
// an empty setting would make every policy match nothing, which reads as "this account
// has no data" rather than as the bug it is.
var ErrNoAccount = errors.New("tenancy: refusing to open a transaction for the nil account")

// InTx runs fn inside a transaction scoped to accountID. The transaction commits when fn
// returns nil and rolls back otherwise. Every RLS policy in the schema evaluates against
// the setting applied here, so a query fn forgets to scope returns an empty result rather
// than another account's rows.
func InTx(
	ctx context.Context,
	db Beginner,
	accountID uuid.UUID,
	fn func(*store.Queries) error,
) error {
	return InTxRaw(ctx, db, accountID, func(tx pgx.Tx) error {
		return fn(store.New(tx))
	})
}

// InTxRaw is InTx without the sqlc layer. Production code uses InTx; this exists for
// schema-adjacent work and for tests that must issue deliberately unscoped SQL to prove
// the RLS backstop holds on its own.
func InTxRaw(
	ctx context.Context,
	db Beginner,
	accountID uuid.UUID,
	fn func(pgx.Tx) error,
) error {
	if accountID == uuid.Nil {
		return ErrNoAccount
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenancy: begin: %w", err)
	}
	// A no-op once the transaction has committed; on any early return it is what actually
	// releases the connection.
	defer func() { _ = tx.Rollback(ctx) }()

	// set_config's third argument is is_local: the setting is scoped to this transaction
	// and released on commit or rollback, so a pooled connection cannot carry it into the
	// next request. SET LOCAL cannot be parameterized, which is why set_config is used --
	// the account id never reaches the SQL text.
	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.account_id', $1, true)`, accountID.String(),
	); err != nil {
		return fmt.Errorf("tenancy: bind account %s: %w", accountID, err)
	}

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenancy: commit: %w", err)
	}
	return nil
}
