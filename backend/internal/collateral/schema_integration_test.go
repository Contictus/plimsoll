//go:build integration

package collateral_test

import (
	"context"
	"os"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/stretchr/testify/require"
)

// L12, for the three tables M5 adds. A snapshot is not a projection, but it is very much
// tenant data: it says what one account holds and how close it is to losing it.
func TestTheCollateralTablesHaveRLSEnabledAndForced(t *testing.T) {
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	defer pool.Close()

	for _, table := range []string{
		"collateral_snapshots", "collateral_positions", "leverage_brackets",
	} {
		var enabled, forced bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`,
			table).Scan(&enabled, &forced), table)
		require.True(t, enabled, "%s: RLS not enabled", table)
		require.True(t, forced, "%s: RLS not forced", table)
	}
}

// A bracket whose cap is not above its floor is a table that cannot be walked: the
// half-open [floor, cap) lookup would match nothing, and MaintenanceAt would report a
// notional outside the brackets for a position squarely inside them.
func TestABracketMustSpanUpward(t *testing.T) {
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	defer pool.Close()

	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'brackets_span_upward')`,
	).Scan(&exists))
	require.True(t, exists, "a bracket table with an inverted tier would be silently unwalkable")
}
