//go:build integration

package valuation_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

var runAt = time.Date(2026, 9, 9, 15, 0, 0, 0, time.UTC)

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

// seedAsset and seedPair build a private corner of the shared registry. The registry has no
// RLS by design (00004), so every test sees every asset -- which is why each one uses
// unique symbols and asserts on its own ids rather than on counts.
func seedAsset(t *testing.T, prefix string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		prefix+"-"+uuid.NewString()).Scan(&id))
	return id
}

func seedPair(t *testing.T, base, quote int64, price string, at time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	var instrumentID int64
	require.NoError(t, ownerPool(t).QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id)
		 VALUES ($1, 'spot', $2, $3) RETURNING id`,
		"V-"+uuid.NewString(), base, quote).Scan(&instrumentID))
	require.NoError(t, ownerPool(t).QueryRow(ctx,
		`INSERT INTO price_ticks (instrument_id, ts, price, source, observed_at)
		 VALUES ($1, $2, $3, 'test', $2) RETURNING instrument_id`,
		instrumentID, at, decimal.RequireFromString(price)).Scan(&instrumentID))
	return instrumentID
}

// A run records every asset it priced, with the path that got there -- which is what makes
// GET /positions/{id}/lineage able to answer "which prices produced this number".
func TestARunRecordsEveryAssetItPricedWithItsPath(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)

	usdc := seedAsset(t, "RUSDC")
	btc := seedAsset(t, "RBTC")
	seedPair(t, btc, usdc, "78000", runAt.Add(-time.Minute))

	pegs := valuation.PegSet{usdc: decimal.RequireFromString("1")}
	res, err := valuation.Produce(ctx, store.New(pool), "test", runAt, pegs)
	require.NoError(t, err)
	require.Positive(t, res.RunID)

	run, err := valuation.LatestRun(ctx, store.New(pool), runAt)
	require.NoError(t, err)
	require.Equal(t, res.RunID, run.ID)
	require.Equal(t, "USD", run.Numeraire)

	priced, ok := run.Prices[btc]
	require.True(t, ok, "the run must carry a price for an asset it could route")
	require.Equal(t, "78000", priced.USD.String())
	require.Len(t, priced.Path, 2, "one market hop and the assumption that ends it")
	require.Equal(t, btc, priced.Path[0].From)
	require.True(t, priced.Path[1].Assumed)

	// The audit property, end to end through JSONB: multiplying the stored rates must
	// reproduce the stored price. A reader who does not trust the number can check it.
	product := decimal.NewFromInt(1)
	for _, hop := range priced.Path {
		product = product.Mul(hop.Rate)
	}
	require.Equal(t, priced.USD.String(), product.Round(18).String())
}

// With one venue and no fiat market every path ends in an assumption, so this is true of
// every run M4 can produce -- and the run says so rather than leaving a reader to infer it.
func TestARunWithAnAssumedLegSaysSoAtTheRunLevel(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)

	usdc := seedAsset(t, "AUSDC")
	btc := seedAsset(t, "ABTC")
	seedPair(t, btc, usdc, "78000", runAt.Add(-time.Minute))

	res, err := valuation.Produce(ctx, store.New(pool), "test", runAt,
		valuation.PegSet{usdc: decimal.RequireFromString("1")})
	require.NoError(t, err)

	run, err := valuation.LatestRun(ctx, store.New(pool), runAt)
	require.NoError(t, err)
	require.Equal(t, res.RunID, run.ID)
	require.True(t, run.AssumedPeg, "a run with an assumed leg must be flagged at the run level")
	require.True(t, run.Prices[btc].AssumedPeg)
}

// The age of a run is the age of its worst leg, never the average. A run whose BTC price is
// a minute old and whose DOGE price is a day old is a day old.
func TestTheRunsAgeIsItsWorstLeg(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)

	usdc := seedAsset(t, "WUSDC")
	fresh := seedAsset(t, "WFRESH")
	stale := seedAsset(t, "WSTALE")

	staleAt := runAt.Add(-24 * time.Hour)
	seedPair(t, fresh, usdc, "10", runAt.Add(-time.Minute))
	seedPair(t, stale, usdc, "1", staleAt)

	_, err := valuation.Produce(ctx, store.New(pool), "test", runAt,
		valuation.PegSet{usdc: decimal.RequireFromString("1")})
	require.NoError(t, err)

	run, err := valuation.LatestRun(ctx, store.New(pool), runAt)
	require.NoError(t, err)
	require.Equal(t, staleAt, run.OldestObservedAt.UTC(),
		"a run is as old as its oldest price, not as young as its newest")
}

// An asset with no route is counted rather than dropped. The caller turns that into "worth
// X, minus the part we could not price" -- a true sentence, where silently omitting it would
// make "worth X" a false one.
func TestAnUnpriceableAssetIsReportedRatherThanDropped(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)

	usdc := seedAsset(t, "UUSDC")
	orphan := seedAsset(t, "UORPHAN")

	res, err := valuation.Produce(ctx, store.New(pool), "test", runAt,
		valuation.PegSet{usdc: decimal.RequireFromString("1")})
	require.NoError(t, err)
	require.Contains(t, res.Unpriceable, orphan)

	run, err := valuation.LatestRun(ctx, store.New(pool), runAt)
	require.NoError(t, err)
	_, present := run.Prices[orphan]
	require.False(t, present, "an asset with no route must not appear with a made-up price")
}
