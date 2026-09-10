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

// The walk asks one question per direction, because `type` is a required parameter and is a
// direction rather than a category (F10). Eight scopes rather than one, so an interrupted
// import resumes per direction instead of restarting all eight.
func TestTransfersWalkOneScopePerDirection(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{}
	d := newDeps(t, client, coinRegistry(t, "USDT"))

	require.NoError(t, backfill.WalkTransfers(ctx, d, target, now.Add(-30*24*time.Hour)))

	asked := map[string]bool{}
	for _, q := range client.transferCalls {
		asked[q.Type] = true
	}
	for _, transferType := range binance.WalkedTransferTypes() {
		require.True(t, asked[transferType], "the walk never asked for %s", transferType)
		require.NotNil(t,
			progressOf(t, d, target, backfill.ScopeTransfers(transferType)).CompletedAt,
			"%s has no completed scope", transferType)
	}
}

// Paging here is `current`/`size` over a window that is still receiving rows -- offset
// paging, which shifts pages under the reader (F10). The walk pins a time window around it,
// and the windows must be contiguous: a gap between two of them is a transfer nobody ever
// reads, and nothing downstream would report its absence.
func TestTransferWindowsAreContiguousAndReachThePresent(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{}
	d := newDeps(t, client, coinRegistry(t, "USDT"))

	since := now.Add(-100 * 24 * time.Hour)
	require.NoError(t, backfill.WalkTransfers(ctx, d, target, since))

	var windows [][2]time.Time
	for _, q := range client.transferCalls {
		if q.Type != "MAIN_UMFUTURE" {
			continue
		}
		w := [2]time.Time{q.StartTime.UTC(), q.EndTime.UTC()}
		if len(windows) == 0 || windows[len(windows)-1] != w {
			windows = append(windows, w)
		}
	}

	require.NotEmpty(t, windows)
	require.Equal(t, since, windows[0][0], "the first window must begin where the caller asked")
	for i := 0; i < len(windows)-1; i++ {
		require.Equal(t, windows[i][1], windows[i+1][0],
			"windows %d and %d leave a gap: a transfer in it is never read", i, i+1)
	}
	require.Equal(t, now, windows[len(windows)-1][1], "the walk must reach the present")
}

// Quoted from the page: "Support query within the last 6 months only". Asking for more is
// not merely wasteful, it is a claim about history the venue will not answer -- and a walk
// that recorded those empty windows as walked would mark a scope complete over months it
// never saw.
func TestTheWalkDoesNotAskBeyondTheSixMonthHorizon(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{}
	d := newDeps(t, client, coinRegistry(t, "USDT"))

	require.NoError(t, backfill.WalkTransfers(ctx, d, target, now.Add(-3*365*24*time.Hour)))

	require.NotEmpty(t, client.transferCalls)
	earliest := client.transferCalls[0].StartTime.UTC()
	for _, q := range client.transferCalls {
		if q.StartTime.UTC().Before(earliest) {
			earliest = q.StartTime.UTC()
		}
	}
	require.Equal(t, now.Add(-backfill.TransferHistory).UTC(), earliest,
		"the walk asked for history the endpoint does not serve")
}

// A window holding more rows than one page must be paged until it is exhausted. `size` maxes
// at 100 (F10), so a busy month genuinely spans pages, and stopping at the first one loses
// transfers silently -- an internal transfer folds to nothing on the balance, so no total
// comes out wrong to announce it.
func TestATransferWindowIsPagedUntilItIsExhausted(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{}
	for i := 1; i <= 5; i++ {
		client.transfers = append(client.transfers, fakeTransfer{
			TranID: int64(4000 + i),
			Type:   "MAIN_UMFUTURE",
			Time:   now.Add(-time.Duration(i) * 24 * time.Hour),
			Asset:  "USDT",
			Amount: "1",
		})
	}
	d := newDeps(t, client, coinRegistry(t, "USDT"))
	d.TransferPageSize = 2

	require.NoError(t, backfill.WalkTransfers(ctx, d, target, now.Add(-10*24*time.Hour)))
	require.Len(t, events(t, target), 5, "a page boundary swallowed transfers")
}

// The two properties every other scope has (K26, K33, L5): interrupt the walk, run it again,
// and the ledger holds one row per transfer. Idempotence here is the schema's -- ON CONFLICT
// DO NOTHING on (integration_id, venue_event_id) -- and it holds only because the identity
// is built from exchange fields alone.
func TestATransferWalkResumesAndStoresEachTransferOnce(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{transfers: []fakeTransfer{
		{TranID: 5001, Type: "MAIN_UMFUTURE", Time: now.Add(-5 * 24 * time.Hour), Asset: "USDT", Amount: "1"},
		{TranID: 5002, Type: "UMFUTURE_MAIN", Time: now.Add(-4 * 24 * time.Hour), Asset: "USDT", Amount: "2"},
	}}
	client.failOnTransferType = "UMFUTURE_MAIN"
	d := newDeps(t, client, coinRegistry(t, "USDT"))
	since := now.Add(-30 * 24 * time.Hour)

	require.Error(t, backfill.WalkTransfers(ctx, d, target, since),
		"the injected failure must reach the caller rather than be swallowed")

	// Whatever was committed before the failure stays committed, and its scope is done.
	require.NotNil(t, progressOf(t, d, target, backfill.ScopeTransfers("MAIN_UMFUTURE")).CompletedAt)
	require.Nil(t, progressOf(t, d, target, backfill.ScopeTransfers("UMFUTURE_MAIN")).CompletedAt)

	client.failOnTransferType = ""
	require.NoError(t, backfill.WalkTransfers(ctx, d, target, since))

	require.ElementsMatch(t, []string{
		binance.TransferID("MAIN_UMFUTURE", 5001),
		binance.TransferID("UMFUTURE_MAIN", 5002),
	}, venueIDs(events(t, target)), "the resumed walk duplicated or dropped a transfer")

	// And a third run, over the same history, adds nothing.
	require.NoError(t, backfill.WalkTransfers(ctx, d, target, since))
	require.Len(t, events(t, target), 2)
}

// F13, through the walk: tranId is documented per row and nothing claims it is unique across
// directions, so two transfers sharing a number must remain two events. An identity built
// from the number alone would merge a move into futures with the move back out -- and the
// survivor would be whichever direction happened to be walked first.
func TestTwoDirectionsSharingATranIDStayTwoEvents(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{transfers: []fakeTransfer{
		{TranID: 6001, Type: "MAIN_UMFUTURE", Time: now.Add(-3 * 24 * time.Hour), Asset: "USDT", Amount: "1"},
		{TranID: 6001, Type: "UMFUTURE_MAIN", Time: now.Add(-2 * 24 * time.Hour), Asset: "USDT", Amount: "1"},
	}}
	d := newDeps(t, client, coinRegistry(t, "USDT"))

	require.NoError(t, backfill.WalkTransfers(ctx, d, target, now.Add(-30*24*time.Hour)))
	require.Len(t, events(t, target), 2, "one tranId in two directions collapsed into one event")
}

// A row that is not CONFIRMED is skipped rather than stored, the way an unsettled deposit
// is, and for the same reason: the ledger is append-only, so recording a movement that has
// not happened could only be undone by a second event reversing the first (L2, F11).
func TestAnUnconfirmedTransferIsSkippedRatherThanStored(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := &fakeClient{transfers: []fakeTransfer{
		{TranID: 7001, Type: "MAIN_UMFUTURE", Time: now.Add(-3 * 24 * time.Hour), Asset: "USDT", Amount: "1"},
		{TranID: 7002, Type: "MAIN_UMFUTURE", Time: now.Add(-2 * 24 * time.Hour), Asset: "USDT", Amount: "9", Status: "PENDING"},
	}}
	d := newDeps(t, client, coinRegistry(t, "USDT"))

	require.NoError(t, backfill.WalkTransfers(ctx, d, target, now.Add(-30*24*time.Hour)))
	require.Equal(t, []string{binance.TransferID("MAIN_UMFUTURE", 7001)}, venueIDs(events(t, target)))
}
