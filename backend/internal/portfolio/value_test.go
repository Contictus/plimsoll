package portfolio_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/stretchr/testify/require"
)

// Asset ids, named so a failing assertion says which asset it was about.
const (
	btcID  int64 = 1
	usdtID int64 = 2
	ethID  int64 = 3
	bnbID  int64 = 4
	dogeID int64 = 5
)

const priceTTL = 5 * time.Minute

// pricedRun builds a valuation as of the read time with every leg observed then, which is
// the uninteresting case: tests that care about age or assumption say so explicitly.
func pricedRun(prices map[int64]string) *valuation.Run {
	out := &valuation.Run{
		ID: 7, AsOf: buildAsOf, Numeraire: "USD", PriceSource: "binance:spot",
		OldestObservedAt: buildAsOf, Prices: map[int64]valuation.RecordedPrice{},
	}
	for id, p := range prices {
		out.Prices[id] = valuation.RecordedPrice{AssetID: id, USD: dec(p), ObservedAt: buildAsOf}
	}
	return out
}

func balance(asset string, id int64, qty string) portfolio.Balance {
	return portfolio.Balance{
		IntegrationID: oneAcct, AssetID: id, Asset: asset,
		Quantity: dec(qty), LastEventTime: buildAsOf,
	}
}

// valuedPosition is one BTC-USDT position; the numbers are arguments because the tests that
// use it care about different ones.
func valuedPosition(qty, entry string) portfolio.Position {
	p := holding(oneAcct, 1, "BTC-USDT", "USDT", qty, entry, "0")
	p.BaseAssetID, p.QuoteAssetID = btcID, usdtID
	p.BaseAsset = "BTC"
	return p
}

func codesOf(r freshness.Report) []string {
	out := make([]string, 0, len(r.Reasons))
	for _, reason := range r.Reasons {
		out = append(out, reason.Code)
	}
	return out
}

func hasCode(r freshness.Report, code string) bool {
	for _, reason := range r.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

// THE TEST TASK 5 EXISTS FOR.
//
// The total is the sum of what is held, valued in one numeraire from one run (L10). It is
// deliberately not the sum of the positions' market values: a position and the balance it
// moved are two views of the same trade, and adding them counts the account's money twice.
func TestTheTotalIsTheSumOfValuedBalances(t *testing.T) {
	got := portfolio.Build(portfolio.Input{
		AsOf:      buildAsOf,
		PriceTTL:  priceTTL,
		Valuation: pricedRun(map[int64]string{btcID: "60000", usdtID: "1.0002", ethID: "2500"}),
		Balances: []portfolio.Balance{
			balance("BTC", btcID, "0.5"),
			balance("USDT", usdtID, "1000"),
			balance("ETH", ethID, "2"),
		},
		// A position over the same assets must not add anything to the total.
		Positions: []portfolio.Position{valuedPosition("0.5", "50000")},
	})

	require.True(t, got.TotalValueUSD.Valid)
	require.Equal(t, "36000.2", got.TotalValueUSD.Decimal.String(),
		"0.5*60000 + 1000*1.0002 + 2*2500")
	require.Equal(t, "30000", got.Balances[0].ValueUSD.Decimal.String())
	require.Equal(t, freshness.StatusOK, got.Freshness.Status)
	require.Empty(t, got.Unpriced)
}

// A position is marked to market in USD, and its unrealized PnL is measured against what it
// cost in the quote asset -- converted at the same run, never at a second price source.
func TestAPositionIsMarkedToMarketAgainstItsEntryInUSD(t *testing.T) {
	got := portfolio.Build(portfolio.Input{
		AsOf:      buildAsOf,
		PriceTTL:  priceTTL,
		Valuation: pricedRun(map[int64]string{btcID: "60000", usdtID: "1"}),
		Positions: []portfolio.Position{valuedPosition("0.5", "50000")},
	})

	require.Equal(t, "30000", got.Holdings[0].MarketValue.Decimal.String())
	require.Equal(t, "5000", got.Holdings[0].UnrealizedPnL.Decimal.String(),
		"0.5*(60000-50000), in USD")
}

// A short's unrealized PnL rises when the market falls. Signing this wrong is invisible in
// a long-only test suite and wrong in exactly the account that most needs it right.
func TestAShortGainsWhenTheMarketFalls(t *testing.T) {
	got := portfolio.Build(portfolio.Input{
		AsOf:      buildAsOf,
		PriceTTL:  priceTTL,
		Valuation: pricedRun(map[int64]string{btcID: "40000", usdtID: "1"}),
		Positions: []portfolio.Position{valuedPosition("-0.5", "50000")},
	})

	require.Equal(t, "5000", got.Holdings[0].UnrealizedPnL.Decimal.String())
	require.Equal(t, "-20000", got.Holdings[0].MarketValue.Decimal.String(),
		"a short's market value is negative: it is exposure owed, not money held")
}

// An assumed peg is disclosed and nothing more. Every number in the response is still
// arithmetic on real prices except the one leg that was assumed, and marking such a
// response degraded would spend the word on the ordinary case (L11).
func TestAnAssumedPegIsDisclosedWithoutDegradingTheResponse(t *testing.T) {
	v := pricedRun(map[int64]string{usdtID: "1"})
	v.AssumedPeg = true

	got := portfolio.Build(portfolio.Input{
		AsOf: buildAsOf, PriceTTL: priceTTL, Valuation: v,
		Balances: []portfolio.Balance{balance("USDT", usdtID, "100")},
	})

	require.Contains(t, codesOf(got.Freshness), freshness.ReasonAssumedPeg)
	require.Equal(t, freshness.StatusOK, got.Freshness.Status)
	require.True(t, got.TotalValueUSD.Valid)
}

// A run whose worst leg is older than tolerance is reported. The total is still served --
// it is the best answer available -- but a reader is told how old the worst of it is.
func TestAStaleRunIsReportedRatherThanHidden(t *testing.T) {
	v := pricedRun(map[int64]string{btcID: "60000"})
	v.OldestObservedAt = buildAsOf.Add(-30 * time.Minute)

	got := portfolio.Build(portfolio.Input{
		AsOf: buildAsOf, PriceTTL: priceTTL, Valuation: v,
		Balances: []portfolio.Balance{balance("BTC", btcID, "1")},
	})

	require.Contains(t, codesOf(got.Freshness), freshness.ReasonPriceStale)
	require.Equal(t, freshness.StatusDegraded, got.Freshness.Status)
	require.True(t, got.TotalValueUSD.Valid, "a stale price is still an answer; silence is not")
}

// THE OTHER TEST TASK 5 EXISTS FOR.
//
// An asset with no route to USD must not quietly leave the sum. "Your portfolio is worth X"
// and "worth X, minus the part we could not price" are different sentences and only one of
// them is true, so the unpriced part is named in the response and raised as an error (L11).
func TestAnUnpriceableHoldingIsNamedRatherThanDroppedFromTheTotal(t *testing.T) {
	got := portfolio.Build(portfolio.Input{
		AsOf: buildAsOf, PriceTTL: priceTTL,
		Valuation: pricedRun(map[int64]string{btcID: "60000"}),
		Balances: []portfolio.Balance{
			balance("BTC", btcID, "1"),
			balance("DOGE", dogeID, "100000"),
		},
	})

	require.Equal(t, []string{"DOGE"}, got.Unpriced)
	require.Contains(t, codesOf(got.Freshness), freshness.ReasonUnknownSymbol)
	require.Equal(t, freshness.StatusUnreliable, got.Freshness.Status)
	require.Equal(t, "60000", got.TotalValueUSD.Decimal.String(),
		"the total is what was priced, and the response says what it excludes")
	require.False(t, got.Balances[1].ValueUSD.Valid, "an unpriced balance has no value, not a zero")
}

// An asset held in zero quantity cannot be priced and does not need to be. Reporting it
// would be noise, and noise in freshness erodes it exactly as fast as silence does.
func TestAZeroBalanceInAnUnpriceableAssetIsNotAFault(t *testing.T) {
	got := portfolio.Build(portfolio.Input{
		AsOf: buildAsOf, PriceTTL: priceTTL,
		Valuation: pricedRun(map[int64]string{btcID: "60000"}),
		Balances:  []portfolio.Balance{balance("DOGE", dogeID, "0")},
	})

	require.Empty(t, got.Unpriced)
	require.False(t, hasCode(got.Freshness, freshness.ReasonUnknownSymbol))
}

// A fee paid in an asset the run could not price leaves the fee totals exact and
// unconvertible. It is a warning, not an error: every other number is intact (L9).
func TestAFeeAssetWithNoPriceIsReported(t *testing.T) {
	got := portfolio.Build(portfolio.Input{
		AsOf: buildAsOf, PriceTTL: priceTTL,
		Valuation: pricedRun(map[int64]string{btcID: "60000"}),
		FeeAssets: []portfolio.FeeAsset{{ID: bnbID, Symbol: "BNB"}, {ID: btcID, Symbol: "BTC"}},
	})

	require.Contains(t, codesOf(got.Freshness), freshness.ReasonFeePriceMissing)
	require.Equal(t, freshness.StatusDegraded, got.Freshness.Status)
}

// The narrowing this task performs. valuation_unavailable no longer means "M4 is not
// built"; it means no run has completed, which is true of a fresh install whose feed has
// never connected -- and a response that dropped the reason would serve a confident zero.
func TestWithoutARunThereIsNoTotalAndTheResponseSaysWhy(t *testing.T) {
	got := portfolio.Build(portfolio.Input{
		AsOf: buildAsOf, PriceTTL: priceTTL,
		Balances: []portfolio.Balance{balance("BTC", btcID, "1")},
	})

	require.False(t, got.TotalValueUSD.Valid, "no run means no total, not a zero")
	require.Contains(t, codesOf(got.Freshness), freshness.ReasonValuationUnavailable)
	require.Nil(t, got.Prices)
}

// The other half of the narrowing: once a run backs the response the reason is gone.
func TestWithARunValuationUnavailableIsNotRaised(t *testing.T) {
	got := portfolio.Build(portfolio.Input{
		AsOf: buildAsOf, PriceTTL: priceTTL,
		Valuation: pricedRun(map[int64]string{btcID: "60000"}),
		Balances:  []portfolio.Balance{balance("BTC", btcID, "1")},
	})

	require.False(t, hasCode(got.Freshness, freshness.ReasonValuationUnavailable))
}

// L10: as_of is the run's instant, not the moment the response was serialized. A client
// comparing two reads is comparing two valuations, and the field has to mean the same thing
// in both.
func TestAsOfIsTheRunsInstantWhenARunBacksTheResponse(t *testing.T) {
	v := pricedRun(map[int64]string{btcID: "60000"})
	v.AsOf = buildAsOf.Add(-90 * time.Second)

	got := portfolio.Build(portfolio.Input{
		AsOf: buildAsOf, PriceTTL: priceTTL, Valuation: v,
		Balances: []portfolio.Balance{balance("BTC", btcID, "1")},
	})

	require.Equal(t, v.AsOf, got.AsOf)
	require.Equal(t, int64(7), got.Prices.RunID)
	require.Equal(t, "binance:spot", got.Prices.Source)
}

// Two reads of the same rows and the same run must produce the same portfolio, valuation
// included -- otherwise a diff between two responses says nothing about what changed.
func TestValuationIsOrderIndependent(t *testing.T) {
	v := pricedRun(map[int64]string{btcID: "60000", ethID: "2500", usdtID: "1"})
	in := func(b ...portfolio.Balance) portfolio.Input {
		return portfolio.Input{AsOf: buildAsOf, PriceTTL: priceTTL, Valuation: v, Balances: b}
	}
	a, e, u := balance("BTC", btcID, "1"), balance("ETH", ethID, "3"), balance("USDT", usdtID, "5")

	require.Equal(t, portfolio.Build(in(a, e, u)), portfolio.Build(in(u, a, e)))
}

// A run that priced none of what this account holds is not a total of zero. The empty sum
// is arithmetically defensible and reads on screen as an empty account, which is the
// confident-and-wrong answer the envelope exists to refuse (L11).
func TestARunThatPricedNothingTheAccountHoldsProducesNoTotal(t *testing.T) {
	got := portfolio.Build(portfolio.Input{
		AsOf: buildAsOf, PriceTTL: priceTTL,
		Valuation: pricedRun(map[int64]string{btcID: "60000"}),
		Balances:  []portfolio.Balance{balance("DOGE", dogeID, "100000")},
	})

	require.False(t, got.TotalValueUSD.Valid)
	require.Equal(t, []string{"DOGE"}, got.Unpriced)
	require.Equal(t, freshness.StatusUnreliable, got.Freshness.Status)
}

// An account holding nothing is worth nothing, and that zero is an answer rather than a
// gap: without it a new account would be indistinguishable from a broken price feed.
func TestAnEmptyAccountIsWorthZeroRatherThanUnknown(t *testing.T) {
	got := portfolio.Build(portfolio.Input{
		AsOf: buildAsOf, PriceTTL: priceTTL,
		Valuation: pricedRun(map[int64]string{btcID: "60000"}),
	})

	require.True(t, got.TotalValueUSD.Valid)
	require.Equal(t, "0", got.TotalValueUSD.Decimal.String())
}
