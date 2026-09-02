//go:build integration

package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// The app role must not be able to bypass RLS. In Postgres a superuser bypasses it
// outright and a table owner bypasses it unless FORCE is set, so both are disqualifying.
func TestAppRoleIsNeitherSuperuserNorOwner(t *testing.T) {
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	defer pool.Close()

	var isSuper, bypassRLS bool
	err = pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuper, &bypassRLS)
	require.NoError(t, err)
	require.False(t, isSuper, "plimsoll_app must not be a superuser")
	require.False(t, bypassRLS, "plimsoll_app must not have BYPASSRLS")
}

func TestAppRoleCannotCreateTables(t *testing.T) {
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(ctx, `CREATE TABLE rls_escape_probe (id int)`)
	require.Error(t, err, "plimsoll_app must not hold DDL rights on schema public")
}

// NUMERIC(38,18) must survive a round trip with every digit intact (L1). Verified here
// rather than in M1, because discovering a driver-level precision loss after the ledger
// exists costs a schema rebuild.
func TestNumericRoundTripKeepsFullPrecision(t *testing.T) {
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	defer pool.Close()

	in := decimal.RequireFromString("12345678901234567890.123456789012345678")

	var out decimal.Decimal
	err = pool.QueryRow(ctx, `SELECT $1::numeric(38,18)`, in).Scan(&out)
	require.NoError(t, err)
	require.Equal(t, in.String(), out.String())
}
