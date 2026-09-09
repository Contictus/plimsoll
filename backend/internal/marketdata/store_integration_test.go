//go:build integration

package marketdata_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/marketdata"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
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

// seedInstrument creates one pair as the owner. Reference data has no RLS and the app role
// cannot create one (00005), so this is the only way in.
func seedInstrument(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)

	var base, quote, id int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"MB-"+uuid.NewString()).Scan(&base))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'stablecoin') RETURNING id`,
		"MQ-"+uuid.NewString()).Scan(&quote))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id)
		 VALUES ($1, 'spot', $2, $3) RETURNING id`,
		"M-"+uuid.NewString(), base, quote).Scan(&id))
	return id
}

func quote(price string, observedAt time.Time) marketdata.Quote {
	return marketdata.Quote{
		Symbol: "IGNORED", Price: decimal.RequireFromString(price), ObservedAt: observedAt,
	}
}

// K7: minute resolution. A stream pushing every second must leave one row per minute, and
// the row must hold the last price observed in it -- a table that grew per push would be a
// different product's problem inside a week.
func TestAMinuteHoldsOneRowAndItIsTheLastPriceInIt(t *testing.T) {
	ctx := context.Background()
	instrumentID := seedInstrument(t)
	pool := appPool(t)
	w := marketdata.Writer{DB: pool}

	minute := time.Date(2026, 9, 9, 14, 37, 0, 0, time.UTC)
	for i, price := range []string{"60000", "60050", "60123.45"} {
		observed := minute.Add(time.Duration(i*20) * time.Second)
		require.NoError(t, w.Record(ctx, []marketdata.Tick{
			marketdata.TickOf(instrumentID, quote(price, observed), "test"),
		}))
	}

	q := store.New(pool)
	count, err := q.CountPriceTicks(ctx, instrumentID)
	require.NoError(t, err)
	require.Equal(t, int64(1), count, "three pushes in one minute must leave one row")

	got, err := q.GetPriceTickAt(ctx, store.GetPriceTickAtParams{
		InstrumentID: instrumentID, At: minute.Add(time.Minute),
	})
	require.NoError(t, err)
	require.Equal(t, "60123.45", got.Price.String(), "the last price in the minute wins")
	require.Equal(t, minute, got.Ts.UTC())
}

// Frames arrive in whatever order the network delivers them. A delayed push carrying an
// older observation must not rewind the price -- that is a movement the venue never made,
// in a number someone is about to act on. The guard is in the upsert, so it holds even if
// the in-memory Book is bypassed.
func TestALateFrameDoesNotRewindTheStoredPrice(t *testing.T) {
	ctx := context.Background()
	instrumentID := seedInstrument(t)
	pool := appPool(t)
	w := marketdata.Writer{DB: pool}

	minute := time.Date(2026, 9, 9, 15, 0, 0, 0, time.UTC)
	newest := marketdata.TickOf(instrumentID, quote("60100", minute.Add(30*time.Second)), "test")
	stale := marketdata.TickOf(instrumentID, quote("60000", minute.Add(10*time.Second)), "test")

	require.NoError(t, w.Record(ctx, []marketdata.Tick{newest}))
	require.NoError(t, w.Record(ctx, []marketdata.Tick{stale}),
		"a late frame is not an error; it is simply not an update")

	got, err := store.New(pool).GetPriceTickAt(ctx, store.GetPriceTickAtParams{
		InstrumentID: instrumentID, At: minute.Add(time.Minute),
	})
	require.NoError(t, err)
	require.Equal(t, "60100", got.Price.String(), "the older observation overwrote the newer one")
	require.Equal(t, newest.ObservedAt, got.ObservedAt.UTC())
}

// At or before, never after. Reaching forward to the next price is look-ahead bias, and in
// a risk product it is the bug that makes a backtest look brilliant and a liquidation
// arrive unannounced.
func TestThePriceInForceIsTheNewestAtOrBeforeTheInstant(t *testing.T) {
	ctx := context.Background()
	instrumentID := seedInstrument(t)
	pool := appPool(t)
	w := marketdata.Writer{DB: pool}

	base := time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC)
	for i, price := range []string{"100", "200", "300"} {
		require.NoError(t, w.Record(ctx, []marketdata.Tick{
			marketdata.TickOf(instrumentID, quote(price, base.Add(time.Duration(i)*time.Minute)), "test"),
		}))
	}

	q := store.New(pool)
	at, err := q.GetPriceTickAt(ctx, store.GetPriceTickAtParams{
		InstrumentID: instrumentID, At: base.Add(90 * time.Second),
	})
	require.NoError(t, err)
	require.Equal(t, "200", at.Price.String(),
		"an instant inside a minute takes that minute's price, not the next one's")

	_, err = q.GetPriceTickAt(ctx, store.GetPriceTickAtParams{
		InstrumentID: instrumentID, At: base.Add(-time.Minute),
	})
	require.Error(t, err, "before the first price there is no price, and that is not zero")
}

// The app role writes prices at runtime -- the worker connects with it -- but it must never
// be able to erase one. A correction is a newer observation, not a removal (L2's reasoning,
// applied to a table that is not the ledger).
func TestTheApplicationRoleCannotDeleteAPrice(t *testing.T) {
	ctx := context.Background()
	instrumentID := seedInstrument(t)
	pool := appPool(t)

	require.NoError(t, marketdata.Writer{DB: pool}.Record(ctx, []marketdata.Tick{
		marketdata.TickOf(instrumentID, quote("1", time.Now().UTC()), "test"),
	}))

	_, err := pool.Exec(ctx, `DELETE FROM price_ticks WHERE instrument_id = $1`, instrumentID)
	require.Error(t, err, "the application role must hold no DELETE on price_ticks")
}
