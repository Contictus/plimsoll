//go:build integration

package backfill_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/backfill"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/stretchr/testify/require"
)

// tradedAccount is one symbol with n fills, ids 1..n, one hour apart.
func tradedAccount(symbol string, n int) *fakeClient {
	trades := make([]fakeTrade, 0, n)
	for i := 1; i <= n; i++ {
		trades = append(trades, fakeTrade{
			ID:    int64(i),
			Time:  now.Add(-time.Duration(n-i) * time.Hour),
			Price: "100.5",
			Qty:   "0.001",
		})
	}
	return &fakeClient{trades: map[string][]fakeTrade{symbol: trades}}
}

func spotRegistry(t *testing.T, symbols ...string) fakeRegistry {
	t.Helper()
	r := fakeRegistry{instruments: map[string]int64{}, assets: map[string]int64{}}
	for _, s := range symbols {
		r.instruments[s] = seedInstrument(t)
	}
	return r
}

// THE M2 EXIT CRITERION: a backfill resumes after interruption.
//
// Every trade must land exactly once and none may be skipped. Both halves matter: a walk
// that replays is merely slow because the ledger deduplicates on venue identity (L5), but a
// walk that skips leaves a missing acquisition, and a missing acquisition poisons the cost
// basis permanently and cannot be repaired by a later event (K26).
func TestAnInterruptedTradeWalkResumesWithoutLosingATrade(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 6)
	client.failFromID = id64(3)
	d := newDeps(t, client, spotRegistry(t, "BTCUSDT"))

	require.Error(t, backfill.WalkTrades(ctx, d, target, "BTCUSDT"),
		"the walk must surface the interruption rather than report a short history as complete")

	partial := events(t, target)
	require.Len(t, partial, 2, "the pages that committed before the failure must survive")
	require.Equal(t, "2", progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).Cursor)
	require.Nil(t, progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).CompletedAt,
		"an interrupted walk is not a complete one")

	client.failFromID = nil
	require.NoError(t, backfill.WalkTrades(ctx, d, target, "BTCUSDT"))

	want := make([]string, 0, 6)
	for i := 1; i <= 6; i++ {
		want = append(want, binance.SpotTradeID("BTCUSDT", int64(i)))
	}
	require.ElementsMatch(t, want, venueIDs(events(t, target)),
		"every trade exactly once: none skipped, none doubled")
	require.NotNil(t, progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).CompletedAt)

	// A completed scope is not walked again. Re-probing a finished symbol on every run is
	// weight spent to learn nothing, and at 20 per request across thousands of symbols it
	// is what gets the shared IP banned (K24).
	before := len(client.tradeCalls)
	require.NoError(t, backfill.WalkTrades(ctx, d, target, "BTCUSDT"))
	require.Equal(t, before, len(client.tradeCalls), "a completed walk must send no requests")
}

// L5 with real payloads on both sides. The REST walk and the live stream report the same
// fill; venue_event_id is built from exchange fields alone, so the second one is a no-op
// rather than a doubled position.
func TestTheSameFillFromTheWalkAndTheStreamIsStoredOnce(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	registry := spotRegistry(t, "BTCUSDT")
	d := newDeps(t, tradedAccount("BTCUSDT", 2), registry)

	require.NoError(t, backfill.WalkTrades(ctx, d, target, "BTCUSDT"))
	require.Len(t, events(t, target), 2)

	// The same fill as trade id 2, as the user data stream reports it.
	report := json.RawMessage(fmt.Sprintf(
		`{"e":"executionReport","E":%d,"s":"BTCUSDT","S":"BUY","x":"TRADE","X":"FILLED",`+
			`"l":"0.001","L":"100.5","n":"0.001","N":"BNB","T":%d,"t":2,"i":9,"m":false}`,
		now.UnixMilli(), now.UnixMilli()))

	event, err := binance.NormalizeStreamExecutionReport(ctx, registry, binance.IngestContext{
		AccountID:     target.AccountID,
		IntegrationID: target.IntegrationID,
		Source:        binance.SourceStream,
	}, report)
	require.NoError(t, err)

	var inserted int
	require.NoError(t, tenancy.InTx(ctx, appPool(t), target.AccountID,
		func(q *store.Queries) error {
			var err error
			inserted, err = ledger.Append(ctx, q, []ledger.Event{event})
			return err
		}))
	require.Zero(t, inserted, "the stream must not insert a fill the walk already stored")
	require.Len(t, events(t, target), 2)
}

// F5 stands in for a verification we cannot run: no real key exists to settle what
// fromId=0 returns (docs/BINANCE-API-NOTES.md section 5). If it answers with the *newest*
// trades, a walk that assumed otherwise reads one page, finds nothing after it, and records
// a complete history that is missing everything before it -- silently, with plausible
// numbers. So the inference is checked at runtime, and a failure is loud (L11).
func TestWalkStopsWhenFromIDZeroDoesNotReturnTheOldestTrades(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 3)
	client.newestOnZero = true
	d := newDeps(t, client, spotRegistry(t, "BTCUSDT"))

	err := backfill.WalkTrades(ctx, d, target, "BTCUSDT")
	require.ErrorIs(t, err, backfill.ErrIncomplete)
	require.Contains(t, err.Error(), backfill.ReasonIncomplete)

	require.Empty(t, events(t, target),
		"a history we cannot page from the start is not appended in part and called done")
	require.Nil(t, progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).CompletedAt,
		"the scope must stay visibly incomplete so freshness can report it (L11)")
}

// A page and the cursor that describes it are one write. If they were two, a crash between
// them would either lose events (cursor first) or replay them forever (events first), and
// the ledger is append-only so neither is repairable (L2).
//
// The failure is induced on the cursor side: 'trades:btcusdt' fails the scope CHECK in
// 00013, so the progress upsert raises after ledger.Append has already succeeded inside the
// same transaction. If the two were separate transactions the events would be committed.
func TestAPageAndItsCursorCommitTogether(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	d := newDeps(t, tradedAccount("btcusdt", 4), spotRegistry(t, "btcusdt"))

	require.Error(t, backfill.WalkTrades(ctx, d, target, "btcusdt"))
	require.Empty(t, events(t, target),
		"events committed without their cursor would be replayed or, worse, skipped")
}

// Each page must begin where the last one ended. If it does not, the walk is not moving
// through the history the way it believes: the cursor would advance over trades that were
// never read, and the ledger's dedup would hide the repetition while the gap stayed.
// Stopping loudly is the only answer that does not end in a silently short history (L11).
func TestAPageThatDoesNotBeginWhereTheLastOneEndedStopsTheWalk(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 6)
	client.stuckAtStart = true
	d := newDeps(t, client, spotRegistry(t, "BTCUSDT"))

	err := backfill.WalkTrades(ctx, d, target, "BTCUSDT")
	require.ErrorIs(t, err, backfill.ErrIncomplete)
	require.Contains(t, err.Error(), backfill.ReasonIncomplete)

	require.Len(t, events(t, target), 2, "the page that did commit is kept; nothing after it is")
	require.Equal(t, "2", progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).Cursor)
	require.Nil(t, progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).CompletedAt)
}
