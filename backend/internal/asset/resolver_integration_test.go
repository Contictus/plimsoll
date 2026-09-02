//go:build integration

package asset_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/asset"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// The registry is reference data, curated by the owner role: there is no account_id and
// no RLS on it, so these helpers connect as the owner to seed and as the app role to
// read, exactly as production does.
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

func at(year int) time.Time { return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC) }

// seedAsset inserts a canonical asset and removes it afterwards. The symbol is made
// unique per test so a failure cannot cascade into the next one.
func seedAsset(t *testing.T, canonicalSymbol, kind string) int64 {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)

	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, $2) RETURNING id`,
		canonicalSymbol, kind).Scan(&id)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

// seedAlias binds an external symbol to an asset for a half-open time window. A nil bound
// means unbounded, which is what a still-listed symbol has.
func seedAlias(t *testing.T, source, external string, assetID int64, from, to *time.Time) error {
	t.Helper()
	_, err := ownerPool(t).Exec(context.Background(),
		`INSERT INTO asset_aliases (source, external_symbol, asset_id, validity)
		 VALUES ($1, $2, $3, tstzrange($4, $5, '[)'))`,
		source, external, assetID, from, to)
	return err
}

func mustSeedAlias(t *testing.T, source, external string, assetID int64, from, to *time.Time) {
	t.Helper()
	require.NoError(t, seedAlias(t, source, external, assetID, from, to))
}

func ptr(v time.Time) *time.Time { return &v }

// K22/L8: exchanges recycle a symbol after a delisting. Resolving a 2022 event with the
// 2026 mapping attaches a correct quantity to the wrong asset -- the failure that gets
// debugged as a price problem for weeks. The window, not the symbol, is the key.
func TestResolutionUsesTheWindowCoveringTheEventTime(t *testing.T) {
	ctx := context.Background()
	q := store.New(appPool(t))

	first := seedAsset(t, "OLD-"+t.Name(), "native")
	second := seedAsset(t, "NEW-"+t.Name(), "native")
	const symbol = "RECYCLED"

	mustSeedAlias(t, "binance", symbol, first, ptr(at(2020)), ptr(at(2024)))
	mustSeedAlias(t, "binance", symbol, second, ptr(at(2025)), nil)

	got, err := asset.Resolve(ctx, q, "binance", symbol, at(2022))
	require.NoError(t, err)
	require.Equal(t, first, got, "a 2022 event must resolve with the 2022 mapping")

	got, err = asset.Resolve(ctx, q, "binance", symbol, at(2026))
	require.NoError(t, err)
	require.Equal(t, second, got)
}

// An unresolved symbol never gets a guess: it is an error the caller must handle, which
// is what becomes an unknown_symbol data-quality finding in M3.5 (K14).
func TestUnknownSymbolIsAnErrorNotTheNearestMatch(t *testing.T) {
	ctx := context.Background()
	q := store.New(appPool(t))

	id := seedAsset(t, "GAP-"+t.Name(), "native")
	const symbol = "DELISTED"
	mustSeedAlias(t, "binance", symbol, id, ptr(at(2020)), ptr(at(2024)))

	// Inside the delisting gap, where the old window has closed and no new one exists.
	_, err := asset.Resolve(ctx, q, "binance", symbol, at(2024))
	require.ErrorIs(t, err, asset.ErrUnknownSymbol, "the gap must not fall back")

	_, err = asset.Resolve(ctx, q, "binance", "NEVER-LISTED", at(2024))
	require.ErrorIs(t, err, asset.ErrUnknownSymbol)
}

// The window is half-open: an event exactly at valid_to belongs to the next window, not
// this one. Without that, one instant resolves two ways.
func TestWindowBoundariesAreHalfOpen(t *testing.T) {
	ctx := context.Background()
	q := store.New(appPool(t))

	first := seedAsset(t, "LEFT-"+t.Name(), "native")
	second := seedAsset(t, "RIGHT-"+t.Name(), "native")
	const symbol = "BOUNDARY"
	boundary := at(2024)

	mustSeedAlias(t, "binance", symbol, first, ptr(at(2020)), ptr(boundary))
	mustSeedAlias(t, "binance", symbol, second, ptr(boundary), nil)

	got, err := asset.Resolve(ctx, q, "binance", symbol, boundary.Add(-time.Millisecond))
	require.NoError(t, err)
	require.Equal(t, first, got)

	got, err = asset.Resolve(ctx, q, "binance", symbol, boundary)
	require.NoError(t, err)
	require.Equal(t, second, got, "valid_to is exclusive")
}

// The exclusion constraint is what stops a bad backfill corrupting the registry: two
// aliases can never claim the same symbol at the same instant, so resolution is a lookup
// and not a choice.
func TestOverlappingWindowsAreRejectedByTheDatabase(t *testing.T) {
	first := seedAsset(t, "A-"+t.Name(), "native")
	second := seedAsset(t, "B-"+t.Name(), "native")
	const symbol = "CONTESTED"

	mustSeedAlias(t, "binance", symbol, first, ptr(at(2020)), ptr(at(2025)))

	err := seedAlias(t, "binance", symbol, second, ptr(at(2024)), ptr(at(2026)))
	require.Error(t, err, "an overlapping window must be refused")
	require.Contains(t, err.Error(), "asset_aliases", "the constraint must name itself")

	// Abutting windows are not overlapping, so this one must be accepted -- the
	// constraint has to be exact, not merely strict.
	require.NoError(t, seedAlias(t, "binance", symbol, second, ptr(at(2025)), nil))
}

// The same ticker means different things on different venues. Source is part of the key,
// so two sources claiming one symbol at one time is normal, not a conflict.
func TestAliasesAreScopedBySource(t *testing.T) {
	ctx := context.Background()
	q := store.New(appPool(t))

	onBinance := seedAsset(t, "BIN-"+t.Name(), "native")
	onBybit := seedAsset(t, "BYB-"+t.Name(), "native")
	const symbol = "SHARED"

	mustSeedAlias(t, "binance", symbol, onBinance, ptr(at(2020)), nil)
	mustSeedAlias(t, "bybit", symbol, onBybit, ptr(at(2020)), nil)

	got, err := asset.Resolve(ctx, q, "binance", symbol, at(2022))
	require.NoError(t, err)
	require.Equal(t, onBinance, got)

	got, err = asset.Resolve(ctx, q, "bybit", symbol, at(2022))
	require.NoError(t, err)
	require.Equal(t, onBybit, got)
}

// A wrapped asset that does not name its underlying is the identity error K10 exists to
// prevent: WBTC priced as if it were unrelated to BTC.
func TestWrappedAssetMustNameItsUnderlying(t *testing.T) {
	// A chain is supplied so that tokens_carry_a_chain cannot be what fires: this test
	// must fail if and only if the wrapped constraint is missing.
	_, err := ownerPool(t).Exec(context.Background(),
		`INSERT INTO assets (canonical_symbol, kind, chain, is_wrapped)
		 VALUES ($1, 'token', 'ethereum', true)`, "WRAP-"+t.Name())
	require.Error(t, err, "is_wrapped without underlying_asset_id must be refused")
	require.Contains(t, err.Error(), "wrapped_assets_name_their_underlying")
}

// The companion constraint, tested on its own so neither can pass for the other's reason:
// the same ticker and contract address exist on several chains, so a token without one is
// unidentifiable.
func TestTokenMustNameItsChain(t *testing.T) {
	_, err := ownerPool(t).Exec(context.Background(),
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'token')`, "TOK-"+t.Name())
	require.Error(t, err)
	require.Contains(t, err.Error(), "tokens_carry_a_chain")
}

// The registry is curated. The request-serving role reads it and cannot write it, so a
// bug in the ingest path cannot invent an asset to make a symbol resolve.
func TestAppRoleCanReadButNotWriteTheRegistry(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)

	for table, want := range map[string]struct{ read, write bool }{
		"assets":        {read: true, write: false},
		"asset_aliases": {read: true, write: false},
	} {
		var canRead, canWrite bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT has_table_privilege(current_user, $1, 'SELECT'),
			        has_table_privilege(current_user, $1, 'INSERT')`, table,
		).Scan(&canRead, &canWrite))
		require.Equal(t, want.read, canRead, "%s SELECT", table)
		require.Equal(t, want.write, canWrite, "%s INSERT", table)
	}
}
