//go:build integration

package backfill_test

import (
	"context"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/backfill"
	"github.com/stretchr/testify/require"
)

// probedSymbols is every symbol the client was asked about, in order.
func probedSymbols(c *fakeClient) []string {
	out := make([]string, 0, len(c.tradeCalls))
	for _, q := range c.tradeCalls {
		out = append(out, q.Symbol)
	}
	return out
}

// F4: there is no endpoint that returns spot trades across symbols, and every cheap
// discovery has the same hole -- an asset bought and fully sold leaves no trace in current
// balances, in deposits, or in withdrawals. Its only record is its trades, which cannot be
// found without already knowing the symbol. So every symbol is probed once.
//
// Once. A sweep that re-probes thousands of symbols on every run costs 40-60k weight for
// nothing, and the shared per-IP budget is what pays for it (K24).
func TestAnUntradedSymbolIsProbedOnceAndNotProbedAgain(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 2)
	d := newDeps(t, client, spotRegistry(t, "BTCUSDT"))

	traded, err := backfill.Discover(ctx, d, target, []string{"AAAUSDT", "BTCUSDT"})
	require.NoError(t, err)
	require.Equal(t, []string{"BTCUSDT"}, traded)
	require.Equal(t, []string{"AAAUSDT", "BTCUSDT"}, probedSymbols(client))
	require.NotNil(t, progressOf(t, d, target, backfill.ScopeDiscover).CompletedAt)

	before := len(client.tradeCalls)
	again, err := backfill.Discover(ctx, d, target, []string{"AAAUSDT", "BTCUSDT"})
	require.NoError(t, err)
	require.Equal(t, []string{"BTCUSDT"}, again,
		"a completed sweep still reports what it found, from the scopes it opened")
	require.Equal(t, before, len(client.tradeCalls), "a completed sweep must send no requests")
}

// Discovery is the expensive half of a backfill, so an interruption two thousand symbols in
// must not throw those two thousand away.
func TestInterruptedDiscoveryResumesAtTheSymbolItStoppedAt(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("CCCUSDT", 2)
	client.failOnSymbol = "BBBUSDT"
	d := newDeps(t, client, spotRegistry(t, "CCCUSDT"))

	symbols := []string{"AAAUSDT", "BBBUSDT", "CCCUSDT"}
	_, err := backfill.Discover(ctx, d, target, symbols)
	require.Error(t, err)
	require.Equal(t, []string{"AAAUSDT", "BBBUSDT"}, probedSymbols(client))
	require.Equal(t, "AAAUSDT", progressOf(t, d, target, backfill.ScopeDiscover).Cursor,
		"the cursor names the last symbol actually settled, not the one that failed")

	client.failOnSymbol = ""
	client.tradeCalls = nil
	traded, err := backfill.Discover(ctx, d, target, symbols)
	require.NoError(t, err)
	require.Equal(t, []string{"BBBUSDT", "CCCUSDT"}, probedSymbols(client),
		"a resumed sweep must not re-probe what it already settled")
	require.Equal(t, []string{"CCCUSDT"}, traded)
}

// One cursor per integration cannot mean both "which symbol was probed" and "which window
// of deposits was read". Finishing the trade walk must not mark the deposits done, because
// a scope wrongly marked complete is never walked again and the hole is permanent.
func TestProgressIsPerScope(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	d := newDeps(t, tradedAccount("BTCUSDT", 2), spotRegistry(t, "BTCUSDT"))

	require.NoError(t, backfill.WalkTrades(ctx, d, target, "BTCUSDT"))

	require.NotNil(t, progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).CompletedAt)
	require.Nil(t, progressOf(t, d, target, backfill.ScopeDeposits).CompletedAt)
	require.Nil(t, progressOf(t, d, target, backfill.ScopeDiscover).CompletedAt)
	require.Nil(t, progressOf(t, d, target, backfill.ScopeTrades("ETHUSDT")).CompletedAt)
}

// Discovery hands each traded symbol to the trade walk by opening its scope, and must not
// rewind a walk that has since made progress. An operator forcing a fresh sweep -- or a new
// symbol listing that makes one worth running -- would otherwise reset every cursor and
// replay the whole history at weight 20 a page.
func TestRediscoveryDoesNotRewindAWalkAlreadyUnderway(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 6)
	client.failFromID = id64(3)
	d := newDeps(t, client, spotRegistry(t, "BTCUSDT"))

	_, err := backfill.Discover(ctx, d, target, []string{"BTCUSDT"})
	require.NoError(t, err)
	require.Error(t, backfill.WalkTrades(ctx, d, target, "BTCUSDT"))
	require.Equal(t, "2", progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).Cursor)

	forgetScope(t, target, backfill.ScopeDiscover)
	_, err = backfill.Discover(ctx, d, target, []string{"BTCUSDT"})
	require.NoError(t, err)
	require.Equal(t, "2", progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).Cursor,
		"a re-run sweep must not reset a cursor and replay the history behind it")
}
