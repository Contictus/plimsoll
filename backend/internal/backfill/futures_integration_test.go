//go:build integration

package backfill_test

import (
	"context"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/backfill"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// M5 SHIPPED THESE NORMALIZERS WITH NOTHING THAT CALLED THEM.
//
// The futures fill fold and the funding fold both existed, and both were tested, and neither
// was reachable from a running worker: no walk asked the venue for a userTrades page or an
// income page. The perpetual half of the ledger stayed empty while the milestone read as
// complete -- the same defect as M2's projector that nothing called (K38), which is why it is
// worth writing down twice.
//
// This test is the caller. It drives the walk and asserts the events arrive.
func TestTheIncomeWalkFoldsFundingAndOpensTheFillWalks(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	perp := seedPerp(t)

	client := &fakeClient{income: []fakeIncome{
		{Symbol: "BTCUSDT", IncomeType: "FUNDING_FEE", Income: "-1.25", Asset: "USDT",
			At: now.Add(-2 * time.Hour), TranID: 900001},
		{Symbol: "BTCUSDT", IncomeType: "FUNDING_FEE", Income: "0.5", Asset: "USDT",
			At: now.Add(-10 * 24 * time.Hour), TranID: 900002},
	}}
	deps := newDeps(t, client, fakeRegistry{instruments: map[string]int64{"BTCUSDT": perp}})

	require.NoError(t, backfill.WalkIncome(ctx, deps, target))

	got := events(t, target)
	require.Len(t, got, 2, "the funding payments never reached the ledger")
	for _, e := range got {
		require.Equal(t, ledger.TypeFundingPayment, e.EventType)
	}

	// And the symbol is now known to be worth walking for fills.
	scopes, err := backfill.Scopes(ctx, deps, target, "usdm:trades:")
	require.NoError(t, err)
	require.Len(t, scopes, 1,
		"the income walk did not open a fill walk, so no futures fill would ever be read")
	symbol, ok := backfill.FuturesSymbolOf(scopes[0].Scope)
	require.True(t, ok)
	require.Equal(t, "BTCUSDT", symbol)
}

// The three types the fold refuses are skipped by NAME, not by accident: each is a second
// copy of something ingested elsewhere, and folding it would count that thing twice.
func TestTheIncomeWalkSkipsWhatIsAlreadyIngestedElsewhere(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	perp := seedPerp(t)

	client := &fakeClient{income: []fakeIncome{
		{Symbol: "ETHUSDT", IncomeType: "TRANSFER", Income: "100", Asset: "USDT",
			At: now.Add(-time.Hour), TranID: 1},
		{Symbol: "ETHUSDT", IncomeType: "REALIZED_PNL", Income: "25", Asset: "USDT",
			At: now.Add(-time.Hour), TranID: 2},
		{Symbol: "ETHUSDT", IncomeType: "COMMISSION", Income: "-0.4", Asset: "USDT",
			At: now.Add(-time.Hour), TranID: 3},
		{Symbol: "ETHUSDT", IncomeType: "FUNDING_FEE", Income: "-1", Asset: "USDT",
			At: now.Add(-time.Hour), TranID: 4},
	}}
	deps := newDeps(t, client, fakeRegistry{instruments: map[string]int64{"ETHUSDT": perp}})

	require.NoError(t, backfill.WalkIncome(ctx, deps, target))
	require.Len(t, events(t, target), 1,
		"a duplicate cash flow was folded: the futures balance now drifts from the venue's")
}

// An unrecognized income type stops the walk rather than passing through as nothing. An
// unknown cash flow folded as zero is a balance that drifts by exactly the amount nobody
// looked at, which is the failure the negative-balance check exists to catch and would not.
func TestAnUnknownIncomeTypeStopsTheWalk(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	perp := seedPerp(t)

	client := &fakeClient{income: []fakeIncome{
		{Symbol: "SOLUSDT", IncomeType: "MYSTERY_REBATE", Income: "3", Asset: "USDT",
			At: now.Add(-time.Hour), TranID: 7},
	}}
	deps := newDeps(t, client, fakeRegistry{instruments: map[string]int64{"SOLUSDT": perp}})

	require.Error(t, backfill.WalkIncome(ctx, deps, target))
}

// The fill walk fills the ledger, and a second run sends no requests at all.
func TestTheFuturesFillWalkIsResumableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	perp := seedPerp(t)

	client := &fakeClient{futuresTrades: []fakeFuturesTrade{
		{Symbol: "BTCUSDT", ID: 5001, Price: "60000", Qty: "0.5", Side: "buy",
			At: now.Add(-3 * time.Hour)},
		{Symbol: "BTCUSDT", ID: 5002, Price: "61000", Qty: "0.5", Side: "sell",
			At: now.Add(-2 * time.Hour)},
	}}
	deps := newDeps(t, client, fakeRegistry{instruments: map[string]int64{"BTCUSDT": perp}})

	require.NoError(t, backfill.WalkFuturesTrades(ctx, deps, target, "BTCUSDT"))
	require.Len(t, events(t, target), 2)

	calls := len(client.futuresTradeCalls)
	require.NoError(t, backfill.WalkFuturesTrades(ctx, deps, target, "BTCUSDT"))
	require.Equal(t, calls, len(client.futuresTradeCalls),
		"a completed walk asked the venue again")
	require.Len(t, events(t, target), 2, "the second walk duplicated the fills")
}

// The venue answers for three months and no further, so the walk asks for exactly that and
// in windows no wider than the seven days the endpoint accepts. A wider window is not a
// slower request, it is a rejected one.
func TestTheFuturesWalkStaysInsideTheVenuesLimits(t *testing.T) {
	ctx := context.Background()
	target := seedIntegration(t)
	perp := seedPerp(t)

	client := &fakeClient{}
	deps := newDeps(t, client, fakeRegistry{instruments: map[string]int64{"BTCUSDT": perp}})
	require.NoError(t, backfill.WalkFuturesTrades(ctx, deps, target, "BTCUSDT"))

	require.NotEmpty(t, client.futuresTradeCalls)
	horizon := now.Add(-90 * 24 * time.Hour)
	for _, call := range client.futuresTradeCalls {
		require.False(t, call.StartTime.Before(horizon.Add(-time.Minute)),
			"the walk asked for %s, before the venue's three-month horizon", call.StartTime)
		require.LessOrEqual(t, call.EndTime.Sub(call.StartTime), 7*24*time.Hour,
			"a window wider than seven days is rejected, not slow")
	}
}

// seedPerp inserts a perpetual. The symbol the fake exchange uses is a fixture detail; what
// the walk resolves is the alias, and the registry here is faked -- so the instrument only
// has to exist for the ledger's foreign key.
func seedPerp(t *testing.T) int64 {
	t.Helper()
	base, quote := seedAsset(t), seedAsset(t)
	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id,
		                          settle_asset_id)
		 VALUES ($1, 'perp', $2, $3, $3) RETURNING id`,
		"FP-"+uuid.NewString()[:10], base, quote).Scan(&id))
	return id
}
