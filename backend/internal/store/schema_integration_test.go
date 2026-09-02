//go:build integration

package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/stretchr/testify/require"
)

// Every tenant table must have RLS both ENABLED and FORCED. Enabled alone still exempts
// the table owner -- and the owner is exactly who the SECURITY DEFINER auth functions run
// as, so the distinction is not academic (K15).
func TestTenantTablesHaveRLSEnabledAndForced(t *testing.T) {
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	defer pool.Close()

	// invites is deliberately absent: it carries no account_id because it exists before
	// the account does, so it is not tenant data. It is protected by privilege instead --
	// see TestAppRoleHasNoPrivilegesOnInvites.
	for _, table := range []string{"accounts", "sessions"} {
		var enabled, forced bool
		err := pool.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity
			   FROM pg_class WHERE oid = $1::regclass`, table,
		).Scan(&enabled, &forced)
		require.NoError(t, err, "table %s", table)
		require.True(t, enabled, "%s: ROW LEVEL SECURITY not enabled", table)
		require.True(t, forced, "%s: ROW LEVEL SECURITY not forced", table)
	}
}

func TestAppRoleHasNoPrivilegesOnInvites(t *testing.T) {
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	defer pool.Close()

	for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		var has bool
		err := pool.QueryRow(ctx,
			`SELECT has_table_privilege(current_user, 'invites', $1)`, priv,
		).Scan(&has)
		require.NoError(t, err)
		require.False(t, has, "plimsoll_app must hold no %s on invites", priv)
	}
}

// The app role must not be able to create an account for itself; that path exists only
// through the invite flow, which runs as the owner.
func TestAppRoleCannotInsertAccounts(t *testing.T) {
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	defer pool.Close()

	var has bool
	err = pool.QueryRow(ctx,
		`SELECT has_table_privilege(current_user, 'accounts', 'INSERT')`).Scan(&has)
	require.NoError(t, err)
	require.False(t, has, "plimsoll_app must hold no INSERT on accounts")
}

// With no account context set, app_current_account() must return NULL rather than
// raising, so an unset session yields zero rows instead of an error a caller might
// swallow.
func TestAppCurrentAccountIsNullWhenUnset(t *testing.T) {
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	defer pool.Close()

	var got *string
	err = pool.QueryRow(ctx, `SELECT app_current_account()::text`).Scan(&got)
	require.NoError(t, err)
	require.Nil(t, got)
}
