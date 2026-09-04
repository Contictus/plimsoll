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

func coinRegistry(t *testing.T, coins ...string) fakeRegistry {
	t.Helper()
	r := fakeRegistry{instruments: map[string]int64{}, assets: map[string]int64{}}
	for _, c := range coins {
		r.assets[c] = seedAsset(t)
	}
	return r
}

// The deposit endpoint caps its span at 90 days -- a different chunking rule from the
// trades walk, which pages by id. Asserted here rather than trusted from the call site,
// because a window one day too wide is rejected by Binance while a window too narrow is
// accepted and merely slow: only one of the two mistakes announces itself.
func TestDepositsWalkInNinetyDayWindows(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{}
	d := newDeps(t, client, coinRegistry(t, "BNB"))

	since := now.Add(-200 * 24 * time.Hour)
	require.NoError(t, backfill.WalkDeposits(ctx, d, target, since))

	require.NotEmpty(t, client.depositCalls)
	require.Equal(t, since, client.depositCalls[0].StartTime.UTC(),
		"the first window must begin where the caller asked, not at an invented epoch")

	// Distinct windows, in order. Offset paging repeats a window; the boundaries are what
	// this test is about.
	var windows [][2]time.Time
	for _, q := range client.depositCalls {
		w := [2]time.Time{q.StartTime.UTC(), q.EndTime.UTC()}
		if len(windows) == 0 || windows[len(windows)-1] != w {
			windows = append(windows, w)
		}
	}

	require.Len(t, windows, 3, "200 days is two full 90-day windows and a short final one")
	for i, w := range windows {
		span := w[1].Sub(w[0])
		if i < len(windows)-1 {
			require.Equal(t, 90*24*time.Hour, span,
				"window %d must use the full documented span", i)
			require.Equal(t, w[1], windows[i+1][0],
				"windows must be contiguous: a gap is a deposit nobody reads")
			continue
		}
		require.LessOrEqual(t, span, 90*24*time.Hour, "the final window still obeys the cap")
		require.Equal(t, now, w[1], "the walk must reach the present")
	}
}

// The three states the fixture documents, walked end to end: a settled deposit is money in
// the account, a pending one is not, and "credited but cannot withdraw" is (the coins are
// there; only moving them out is blocked, which the ledger does not model).
func TestOnlySettledDepositsBecomeEvents(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{deposits: []fakeDeposit{
		{ID: "1001", Time: now.Add(-40 * 24 * time.Hour), Coin: "BNB", Amount: "0.5", Status: 1},
		{ID: "1002", Time: now.Add(-30 * 24 * time.Hour), Coin: "BNB", Amount: "9", Status: 0},
		{ID: "1003", Time: now.Add(-20 * 24 * time.Hour), Coin: "BNB", Amount: "5", Status: 6},
	}}
	d := newDeps(t, client, coinRegistry(t, "BNB"))

	require.NoError(t, backfill.WalkDeposits(ctx, d, target, now.Add(-60*24*time.Hour)))

	require.ElementsMatch(t,
		[]string{binance.SpotDepositID("1001"), binance.SpotDepositID("1003")},
		venueIDs(events(t, target)),
		"a pending deposit is not money in the account, and the ledger is append-only (L2)")
	require.NotNil(t, progressOf(t, d, target, backfill.ScopeDeposits).CompletedAt)
}

// A window holding more rows than one page must be paged by offset until it is exhausted.
// Stopping at the first page loses deposits, and a lost deposit is a balance that never
// arrives -- the same permanent hole a missing trade leaves (K26).
func TestAWindowIsPagedUntilItIsExhausted(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{}
	for i := 1; i <= 5; i++ {
		client.deposits = append(client.deposits, fakeDeposit{
			ID:     "200" + string(rune('0'+i)),
			Time:   now.Add(-time.Duration(i) * 24 * time.Hour),
			Coin:   "BNB",
			Amount: "1",
			Status: 1,
		})
	}
	d := newDeps(t, client, coinRegistry(t, "BNB"))
	d.DepositPageLimit = 2

	require.NoError(t, backfill.WalkDeposits(ctx, d, target, now.Add(-10*24*time.Hour)))
	require.Len(t, events(t, target), 5)
}

// A second run starts from the cursor rather than from `since`. Re-reading windows already
// settled is weight spent to learn nothing, and on an account with years of history it is
// most of the walk.
func TestASecondDepositWalkResumesAtItsCursor(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{deposits: []fakeDeposit{
		{ID: "3001", Time: now.Add(-150 * 24 * time.Hour), Coin: "BNB", Amount: "1", Status: 1},
		{ID: "3002", Time: now.Add(-10 * 24 * time.Hour), Coin: "BNB", Amount: "2", Status: 1},
	}}
	d := newDeps(t, client, coinRegistry(t, "BNB"))
	since := now.Add(-200 * 24 * time.Hour)

	require.NoError(t, backfill.WalkDeposits(ctx, d, target, since))
	require.Len(t, events(t, target), 2)
	require.NotNil(t, progressOf(t, d, target, backfill.ScopeDeposits).CompletedAt)

	// Time moves on and the walk is run again, as the supervisor will run it.
	later := now.Add(30 * 24 * time.Hour)
	d.Now = func() time.Time { return later }
	client.depositCalls = nil
	require.NoError(t, backfill.WalkDeposits(ctx, d, target, since))

	require.NotEmpty(t, client.depositCalls)
	require.Equal(t, now, client.depositCalls[0].StartTime.UTC(),
		"the resumed walk begins at the cursor the last run left, not at `since`")
	require.Equal(t, later, client.depositCalls[0].EndTime.UTC())
}
