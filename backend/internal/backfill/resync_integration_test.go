//go:build integration

package backfill_test

import (
	"context"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/backfill"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/stretchr/testify/require"
)

// A gap replay reads by time, not by id: myTrades does not accept fromId together with a
// time range, and the enumeration of supported combinations in rest-api.md is the whole
// reason the walk and the replay are two different strategies rather than one.
func TestAResyncReadsTheWindowByTimeAndAppendsWhatItFinds(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 4)
	d := newDeps(t, client, spotRegistry(t, "BTCUSDT"))

	// One second past the newest trade: the boundary is deliberately overshot. Whether the
	// venue treats endTime as inclusive is not stated, and a window that overlaps its
	// neighbour costs a deduplicated row while one that falls short costs a lost trade.
	from, to := now.Add(-4*time.Hour), now.Add(time.Second)
	require.NoError(t, backfill.ResyncWindow(ctx, d, target, []string{"BTCUSDT"}, from, to))

	require.Len(t, events(t, target), 4)
	require.NotEmpty(t, client.tradeCalls)
	for _, q := range client.tradeCalls {
		require.Nil(t, q.FromID, "a windowed read must not also send fromId")
		require.False(t, q.StartTime.IsZero(), "a replay is bounded by time")
		require.False(t, q.EndTime.IsZero())
	}
}

// The replay must not move the walk's cursor. They are different readings of the same
// history: the walk owns "how far back have we read", and a replay that touched it would
// declare a symbol's history complete after reading ten minutes of it.
func TestAResyncLeavesTheWalkCursorAlone(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 4)
	d := newDeps(t, client, spotRegistry(t, "BTCUSDT"))

	before := progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT"))
	require.NoError(t, backfill.ResyncWindow(
		ctx, d, target, []string{"BTCUSDT"}, now.Add(-4*time.Hour), now))

	after := progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT"))
	require.Equal(t, before.Cursor, after.Cursor)
	require.Nil(t, after.CompletedAt, "a replay of ten minutes does not complete a history")
}

// The same trade seen by the walk and by a replay is one row. Overlap is the normal case --
// a gap window deliberately overshoots -- so dedup on venue identity is what makes replay
// safe to run generously (L5).
func TestAResyncOverlappingTheWalkStoresNothingTwice(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 4)
	d := newDeps(t, client, spotRegistry(t, "BTCUSDT"))

	require.NoError(t, backfill.WalkTrades(ctx, d, target, "BTCUSDT"))
	require.Len(t, events(t, target), 4)

	require.NoError(t, backfill.ResyncWindow(
		ctx, d, target, []string{"BTCUSDT"}, now.Add(-4*time.Hour), now))
	require.Len(t, events(t, target), 4, "a replay of what we already have adds nothing")
}

// A window holding more trades than one page cannot be paged by id -- the venue will not
// take fromId with a time range -- so it is halved until each half fits. Halving is what
// keeps the replay complete without assuming a parameter combination the docs do not list.
func TestAFullWindowIsHalvedRatherThanPagedByID(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 6)
	d := newDeps(t, client, spotRegistry(t, "BTCUSDT")) // page limit 2

	require.NoError(t, backfill.ResyncWindow(
		ctx, d, target, []string{"BTCUSDT"}, now.Add(-8*time.Hour), now.Add(time.Second)))

	require.Len(t, events(t, target), 6, "every trade in the window must be read")
	for _, q := range client.tradeCalls {
		require.Nil(t, q.FromID)
	}
	require.Greater(t, len(client.tradeCalls), 1, "one full page must have forced a split")
}

// The venue answers at most 24 hours. A wider window is refused here rather than sent and
// rejected, because a rejected replay reads as an empty one.
func TestAResyncRefusesAWindowWiderThanTheVenueAllows(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	d := newDeps(t, tradedAccount("BTCUSDT", 1), spotRegistry(t, "BTCUSDT"))

	err := backfill.ResyncWindow(ctx, d, target, []string{"BTCUSDT"},
		now.Add(-binance.MaxTradeWindow-time.Minute), now)
	require.ErrorIs(t, err, binance.ErrUnsupportedQuery)
}

// Deposits arrive in the gap too, and they page by offset within a pinned window exactly as
// the backfill's do.
func TestAResyncAlsoReadsDepositsInTheWindow(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{deposits: []fakeDeposit{
		{ID: "9001", Time: now.Add(-2 * time.Hour), Coin: "BNB", Amount: "3", Status: 1},
	}}
	d := newDeps(t, client, coinRegistry(t, "BNB"))

	require.NoError(t, backfill.ResyncWindow(ctx, d, target, nil, now.Add(-4*time.Hour), now))
	require.Equal(t, []string{binance.SpotDepositID("9001")}, venueIDs(events(t, target)))
}
