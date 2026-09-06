//go:build integration

package backfill_test

import (
	"context"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/backfill"
	"github.com/stretchr/testify/require"
)

// A backfill is driven one chunk at a time rather than as one call. The supervisor has to
// be able to stop between chunks when its lease is lost, and a single call that ran for an
// hour could not (L6).
func TestTheRunnerDoesOneChunkAtATimeAndEventuallyFinishes(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 4)
	client.deposits = []fakeDeposit{
		{ID: "5001", Time: now.Add(-2 * time.Hour), Coin: "BNB", Amount: "1", Status: 1},
	}
	d := newDeps(t, client, mixedRegistry(t, "BTCUSDT", "BNB"))

	runner := &backfill.Runner{
		Deps: d, Target: target,
		Symbols: []string{"AAAUSDT", "BTCUSDT"},
		Since:   now.Add(-30 * 24 * time.Hour),
	}

	steps := 0
	for {
		more, err := runner.Step(ctx)
		require.NoError(t, err)
		if !more {
			break
		}
		steps++
		require.Less(t, steps, 20, "the runner is not making progress")
	}

	require.Greater(t, steps, 2, "deposits, discovery and one symbol are three distinct chunks")
	require.Len(t, events(t, target), 5, "four fills and one deposit")

	require.NotNil(t, progressOf(t, d, target, backfill.ScopeDeposits).CompletedAt)
	require.NotNil(t, progressOf(t, d, target, backfill.ScopeDiscover).CompletedAt)
	require.NotNil(t, progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).CompletedAt)

	// A finished runner does no further work and sends no further requests.
	before := len(client.tradeCalls) + len(client.depositCalls)
	more, err := runner.Step(ctx)
	require.NoError(t, err)
	require.False(t, more)
	require.Equal(t, before, len(client.tradeCalls)+len(client.depositCalls))
}

// Discovery has to run before the symbol walks, because a trades scope does not exist until
// discovery opens it. A runner that walked first would find nothing to walk and report a
// complete history over an account it never looked at.
func TestTheRunnerDiscoversBeforeItWalks(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	client := tradedAccount("BTCUSDT", 2)
	d := newDeps(t, client, mixedRegistry(t, "BTCUSDT", "BNB"))

	runner := &backfill.Runner{
		Deps: d, Target: target, Symbols: []string{"BTCUSDT"}, Since: now.Add(-24 * time.Hour),
	}

	for {
		more, err := runner.Step(ctx)
		require.NoError(t, err)
		if !more {
			break
		}
		if progressOf(t, d, target, backfill.ScopeTrades("BTCUSDT")).Cursor != "" {
			require.NotNil(t, progressOf(t, d, target, backfill.ScopeDiscover).CompletedAt,
				"a symbol was walked before discovery finished")
		}
	}
	require.Len(t, events(t, target), 2)
}

// mixedRegistry resolves both a trading pair and a coin, which is what a runner needs: one
// walk moves pairs and the other moves single assets.
func mixedRegistry(t *testing.T, symbol, coin string) fakeRegistry {
	t.Helper()
	return fakeRegistry{
		instruments: map[string]int64{symbol: seedInstrument(t)},
		assets:      map[string]int64{coin: seedAsset(t)},
	}
}
