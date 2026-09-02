//go:build integration

package instrument_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

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

func ptr(v time.Time) *time.Time { return &v }

// seedAsset creates one leg of an instrument. Native kind, so no chain is required.
func seedAsset(t *testing.T, symbol string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		symbol).Scan(&id))
	t.Cleanup(func() {
		_, _ = ownerPool(t).Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

func seedInstrument(t *testing.T, canonical string, kind instrument.Kind) int64 {
	t.Helper()
	base, quote := seedAsset(t, canonical+"-BASE"), seedAsset(t, canonical+"-QUOTE")

	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id,
		                          settle_asset_id, contract_size)
		 VALUES ($1, $2, $3, $4, $4, 1) RETURNING id`,
		canonical, string(kind), base, quote).Scan(&id))
	t.Cleanup(func() {
		_, _ = ownerPool(t).Exec(context.Background(), `DELETE FROM instruments WHERE id = $1`, id)
	})
	return id
}

func seedAlias(
	t *testing.T, exchange string, market instrument.Market,
	symbol string, instrumentID int64, from, to *time.Time,
) error {
	t.Helper()
	_, err := ownerPool(t).Exec(context.Background(),
		`INSERT INTO instrument_aliases
		   (exchange, market, exchange_symbol, instrument_id, validity)
		 VALUES ($1, $2, $3, $4, tstzrange($5, $6, '[)'))`,
		exchange, string(market), symbol, instrumentID, from, to)
	return err
}

func mustSeedAlias(
	t *testing.T, exchange string, market instrument.Market,
	symbol string, instrumentID int64, from, to *time.Time,
) {
	t.Helper()
	require.NoError(t, seedAlias(t, exchange, market, symbol, instrumentID, from, to))
}

// THE TEST THIS TASK EXISTS FOR (K10, L8).
//
// On Binance, spot BTCUSDT and perpetual BTCUSDT are the same string and different
// instruments. If market is not part of the lookup the two positions merge, and every
// number downstream -- exposure, leverage, net delta -- is silently wrong while looking
// entirely plausible.
func TestSameSymbolInTwoMarketsResolvesToTwoInstruments(t *testing.T) {
	ctx := context.Background()
	q := store.New(appPool(t))

	spot := seedInstrument(t, "BTC-USDT-SPOT-"+t.Name(), instrument.KindSpot)
	perp := seedInstrument(t, "BTC-USDT-PERP-"+t.Name(), instrument.KindPerp)

	mustSeedAlias(t, "binance", instrument.MarketSpot, "BTCUSDT", spot, ptr(at(2020)), nil)
	mustSeedAlias(t, "binance", instrument.MarketUSDM, "BTCUSDT", perp, ptr(at(2020)), nil)

	gotSpot, err := instrument.Resolve(ctx, q, "binance", instrument.MarketSpot, "BTCUSDT", at(2024))
	require.NoError(t, err)
	gotPerp, err := instrument.Resolve(ctx, q, "binance", instrument.MarketUSDM, "BTCUSDT", at(2024))
	require.NoError(t, err)

	require.Equal(t, spot, gotSpot)
	require.Equal(t, perp, gotPerp)
	require.NotEqual(t, gotSpot, gotPerp, "spot and perp must never collapse into one")
}

// K22/L8, at the instrument layer: an exchange relists a symbol against a different
// contract, and a 2022 trade must not be resolved with the 2026 mapping.
func TestResolutionUsesTheWindowCoveringTheEventTime(t *testing.T) {
	ctx := context.Background()
	q := store.New(appPool(t))

	first := seedInstrument(t, "OLD-"+t.Name(), instrument.KindPerp)
	second := seedInstrument(t, "NEW-"+t.Name(), instrument.KindPerp)

	mustSeedAlias(t, "binance", instrument.MarketUSDM, "RECYCLED", first, ptr(at(2020)), ptr(at(2024)))
	mustSeedAlias(t, "binance", instrument.MarketUSDM, "RECYCLED", second, ptr(at(2025)), nil)

	got, err := instrument.Resolve(ctx, q, "binance", instrument.MarketUSDM, "RECYCLED", at(2022))
	require.NoError(t, err)
	require.Equal(t, first, got)

	got, err = instrument.Resolve(ctx, q, "binance", instrument.MarketUSDM, "RECYCLED", at(2026))
	require.NoError(t, err)
	require.Equal(t, second, got)
}

func TestUnknownSymbolIsAnErrorNotTheNearestMatch(t *testing.T) {
	ctx := context.Background()
	q := store.New(appPool(t))

	id := seedInstrument(t, "GAP-"+t.Name(), instrument.KindSpot)
	mustSeedAlias(t, "binance", instrument.MarketSpot, "DELISTED", id, ptr(at(2020)), ptr(at(2024)))

	_, err := instrument.Resolve(ctx, q, "binance", instrument.MarketSpot, "DELISTED", at(2024))
	require.ErrorIs(t, err, instrument.ErrUnknownSymbol)

	// Right symbol, right time, wrong market: still nothing, and still not a guess.
	_, err = instrument.Resolve(ctx, q, "binance", instrument.MarketUSDM, "DELISTED", at(2022))
	require.ErrorIs(t, err, instrument.ErrUnknownSymbol)
}

// The exclusion constraint must cover market too. Two instruments claiming one symbol in
// one market at one instant is corruption; the same symbol in two markets is normal.
func TestOverlapIsRejectedWithinAMarketAndAllowedAcross(t *testing.T) {
	first := seedInstrument(t, "A-"+t.Name(), instrument.KindSpot)
	second := seedInstrument(t, "B-"+t.Name(), instrument.KindPerp)

	mustSeedAlias(t, "binance", instrument.MarketSpot, "CONTESTED", first, ptr(at(2020)), ptr(at(2025)))

	err := seedAlias(t, "binance", instrument.MarketSpot, "CONTESTED", second, ptr(at(2024)), nil)
	require.Error(t, err, "an overlapping window in the same market must be refused")
	require.Contains(t, err.Error(), "instrument_aliases")

	// The same symbol, the same instant, a different market: this is the Binance case and
	// it must be allowed, or the constraint would forbid the very thing K10 requires.
	require.NoError(t,
		seedAlias(t, "binance", instrument.MarketUSDM, "CONTESTED", second, ptr(at(2024)), nil))
}

// A perp settles in something; a spot pair does not. Getting this wrong is how a
// coin-margined contract gets valued as if it settled in USDT.
func TestPerpMustNameItsSettleAsset(t *testing.T) {
	base, quote := seedAsset(t, "PB-"+t.Name()), seedAsset(t, "PQ-"+t.Name())

	_, err := ownerPool(t).Exec(context.Background(),
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id,
		                          contract_size)
		 VALUES ($1, 'perp', $2, $3, 1)`, "NOSETTLE-"+t.Name(), base, quote)
	require.Error(t, err)
	require.Contains(t, err.Error(), "perps_settle_somewhere")
}

func TestAppRoleCanReadButNotWriteTheRegistry(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)

	for _, table := range []string{"instruments", "instrument_aliases"} {
		var canRead, canWrite bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT has_table_privilege(current_user, $1, 'SELECT'),
			        has_table_privilege(current_user, $1, 'INSERT')`, table,
		).Scan(&canRead, &canWrite))
		require.True(t, canRead, "%s must be readable", table)
		require.False(t, canWrite, "%s must not be writable by the app role", table)
	}
}
