//go:build integration

package tenancy_test

import (
	"context"
	"os"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// asAccount runs fn against a pool with app.account_id bound. The seed and cleanup need
// it because FORCE ROW LEVEL SECURITY applies to the table owner as well -- which is
// itself the first evidence that FORCE is in effect.
func asAccount(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One statement per Exec: pgx uses the extended protocol, which does not accept
	// several parameterized statements in a single call.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.account_id', $1, true)`, id.String()); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func ownerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// seedAccounts inserts two accounts as the owner, bypassing the application layer
// entirely. The point is to create data the app role must not be able to reach.
func seedAccounts(t *testing.T) (a, b uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	owner := ownerPool(t)

	a, b = uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{a, b} {
		email := id.String() + "@example.test"
		require.NoError(t, asAccount(ctx, owner, id, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO accounts (id, email) VALUES ($1, $2)`,
				id, email)
			return err
		}))
	}
	t.Cleanup(func() {
		for _, id := range []uuid.UUID{a, b} {
			_ = asAccount(ctx, owner, id, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, id)
				return err
			})
		}
	})
	return a, b
}

// THE M0 EXIT TEST (K15, L12, ARCHITECTURE.md section 12, test 4).
//
// The query below has NO account_id predicate. The application-level defence is
// deliberately absent, so anything it returns is returned by RLS alone. A test that
// passed with both defences present would have verified neither.
func TestRLSAloneBlocksCrossTenantRead(t *testing.T) {
	ctx := context.Background()
	accountA, accountB := seedAccounts(t)
	pool := appPool(t)

	var seen []uuid.UUID
	err := tenancy.InTxRaw(ctx, pool, accountA, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM accounts`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			seen = append(seen, id)
		}
		return rows.Err()
	})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{accountA}, seen,
		"an unscoped SELECT must still return only the current account")
	require.NotContains(t, seen, accountB)
}

// Reaching the pool without InTx must yield nothing, not everything. This is the failure
// mode the whole design exists to make boring.
func TestQueryOutsideInTxSeesNothing(t *testing.T) {
	seedAccounts(t)
	pool := appPool(t)

	var count int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM accounts`).Scan(&count)
	require.NoError(t, err)
	require.Zero(t, count, "no tenant context set must mean no rows, not all rows")
}

// A write aimed at another account must not land, even though the statement itself is
// syntactically allowed.
func TestCannotWriteIntoAnotherAccount(t *testing.T) {
	ctx := context.Background()
	accountA, accountB := seedAccounts(t)
	pool := appPool(t)

	err := tenancy.InTxRaw(ctx, pool, accountA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE accounts SET email = 'stolen@example.test' WHERE id = $1`, accountB)
		return err
	})
	require.NoError(t, err)

	owner := ownerPool(t)
	var email string
	require.NoError(t, asAccount(ctx, owner, accountB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT email FROM accounts WHERE id = $1`, accountB).Scan(&email)
	}))
	require.NotEqual(t, "stolen@example.test", email,
		"account B was modified from account A's transaction")
}

// The tenant setting must not survive the transaction. A pooled connection handed to the
// next request with a stale account id would be a cross-tenant leak with no symptom.
func TestSettingDoesNotLeakAcrossTransactions(t *testing.T) {
	ctx := context.Background()
	accountA, _ := seedAccounts(t)
	pool := appPool(t)

	require.NoError(t, tenancy.InTxRaw(ctx, pool, accountA, func(tx pgx.Tx) error { return nil }))

	var got *string
	err := pool.QueryRow(ctx, `SELECT app_current_account()::text`).Scan(&got)
	require.NoError(t, err)
	require.Nil(t, got, "app.account_id leaked out of its transaction")
}

// InTx must hand the caller a Queries bound to the transaction, so the generated code
// inherits the tenant scope automatically.
func TestInTxScopesGeneratedQueries(t *testing.T) {
	ctx := context.Background()
	accountA, accountB := seedAccounts(t)
	pool := appPool(t)

	require.NoError(t, tenancy.InTx(ctx, pool, accountA, func(q *store.Queries) error {
		got, err := q.GetAccountByID(ctx, accountA)
		if err != nil {
			return err
		}
		require.Equal(t, accountA, got.ID)
		return nil
	}))

	// Asking for another account's row by id returns no rows rather than the row.
	err := tenancy.InTx(ctx, pool, accountA, func(q *store.Queries) error {
		_, err := q.GetAccountByID(ctx, accountB)
		return err
	})
	require.ErrorIs(t, err, pgx.ErrNoRows)
}

// The nil account is never a legitimate tenant; opening a transaction for it would bind
// an empty setting and quietly match nothing, which is harder to debug than a refusal.
func TestInTxRefusesTheNilAccount(t *testing.T) {
	pool := appPool(t)
	err := tenancy.InTxRaw(context.Background(), pool, uuid.Nil, func(pgx.Tx) error {
		t.Fatal("fn must not run")
		return nil
	})
	require.Error(t, err)
}
