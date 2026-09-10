package risk_test

import (
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/risk"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func value(s string) decimal.NullDecimal { return decimal.NewNullDecimal(d(s)) }

// position is the shape the read model hands the engine: a signed market value in the run's
// numeraire, the base asset it is exposure to, and the group the user put it in.
func position(symbol, base, strategy, marketValue string) risk.Position {
	return risk.Position{
		Symbol:      symbol,
		BaseAsset:   base,
		Strategy:    strategy,
		MarketValue: value(marketValue),
	}
}

func byStrategy(t *testing.T, r risk.Report, name string) risk.Metrics {
	t.Helper()
	for _, s := range r.ByStrategy {
		if s.Strategy == name {
			return s.Metrics
		}
	}
	t.Fatalf("no strategy %q in the report", name)
	return risk.Metrics{}
}

func delta(t *testing.T, m risk.Metrics, asset string) decimal.Decimal {
	t.Helper()
	for _, a := range m.NetDelta {
		if a.Asset == asset {
			return a.Exposure
		}
	}
	t.Fatalf("no net delta for %q", asset)
	return decimal.Decimal{}
}

// THE BASIS TRADE (K13, ARCHITECTURE section 9).
//
// A BTC spot long of +$50,000 against a BTC perp short of -$50,000 is not a 2x leveraged
// account. Its directional risk is about zero; its real risks are funding flips, the short
// leg's liquidation, basis and venue. A system that reports the first number alerts the user
// constantly and incorrectly, and a user who learns to ignore alerts has no alerting.
//
// Both numbers are asserted, deliberately. The wrong one is only wrong RELATIVE to the right
// one, and a test that saw only the right one would pass on an engine that ignored strategies
// entirely -- which is the engine we are trying not to write.
func TestABasisTradeIsNotTwoTimesLeveraged(t *testing.T) {
	report := risk.Compute(risk.Input{
		Equity: d("50000"),
		Positions: []risk.Position{
			position("BTC-USDT", "BTC", "carry", "50000"),
			position("BTC-USDT-PERP", "BTC", "carry", "-50000"),
		},
	})

	// What a system without strategies reports, and it is not wrong -- it is incomplete.
	require.Equal(t, "100000", report.Portfolio.GrossExposure.String())
	require.True(t, report.Portfolio.Leverage.Valid)
	require.Equal(t, "2", report.Portfolio.Leverage.Decimal.String(),
		"gross leverage is 2x, and reporting only this is what generates the false alert")

	// And what the grouping says: no directional exposure at all.
	carry := byStrategy(t, report, "carry")
	require.Equal(t, "0", delta(t, carry, "BTC").String(),
		"the two legs did not cancel: the hedge is invisible to the engine")
	require.Equal(t, "0", carry.NetExposure.String())
	require.True(t, carry.NetLeverage.Valid)
	require.Equal(t, "0", carry.NetLeverage.Decimal.String(),
		"directional leverage inside the strategy is zero; gross alone would say 2x")

	// The portfolio's own net delta is the same zero: the legs cancel wherever they are
	// summed. What the strategy adds is knowing they were MEANT to.
	require.Equal(t, "0", delta(t, report.Portfolio, "BTC").String())
}

// A hedge that is not a hedge must not be flattered by being tagged. Grouping is a claim
// about intent, and the engine reports what the positions actually are.
func TestATaggedPairThatDoesNotCancelStillReportsItsDelta(t *testing.T) {
	report := risk.Compute(risk.Input{
		Equity: d("50000"),
		Positions: []risk.Position{
			position("BTC-USDT", "BTC", "carry", "50000"),
			position("BTC-USDT-PERP", "BTC", "carry", "-10000"),
		},
	})

	carry := byStrategy(t, report, "carry")
	require.Equal(t, "40000", delta(t, carry, "BTC").String(),
		"a broken hedge reported as a working one is worse than no grouping at all")
	require.Equal(t, "0.8", carry.NetLeverage.Decimal.String())
}

// L11, and the same rule as K50's margin buffer: an unpriced position is NAMED, never counted
// as zero. Zero exposure and unknown exposure are opposite claims, and a portfolio that
// silently drops what it could not price reports less risk than it has.
func TestAnUnpricedPositionIsNamedRatherThanCountedAsZero(t *testing.T) {
	report := risk.Compute(risk.Input{
		Equity: d("50000"),
		Positions: []risk.Position{
			position("BTC-USDT", "BTC", "", "50000"),
			{Symbol: "WEIRD-USDT", BaseAsset: "WEIRD", MarketValue: decimal.NullDecimal{}},
		},
	})

	require.Equal(t, "50000", report.Portfolio.GrossExposure.String())
	require.Equal(t, []string{"WEIRD"}, report.Unpriced,
		"an unpriced position vanished from the report instead of being named")
}

// Leverage against zero equity is UNDEFINED, not infinite and not a very large number. An
// account with no equity and an open position is in a state a ratio cannot describe, and
// rendering it as a number teaches a reader to compare it with other numbers.
func TestLeverageWithNoEquityIsUndefined(t *testing.T) {
	report := risk.Compute(risk.Input{
		Equity:    decimal.Zero,
		Positions: []risk.Position{position("BTC-USDT", "BTC", "", "50000")},
	})
	require.False(t, report.Portfolio.Leverage.Valid, "leverage was divided by zero equity")
	require.False(t, report.Portfolio.NetLeverage.Valid)
	require.Equal(t, "50000", report.Portfolio.GrossExposure.String(),
		"the exposure is still known; only the ratio is not")
}

// Concentration is a share of gross exposure and sums to one across a priced portfolio, so a
// single-asset account reads 100% rather than an arbitrary slice of a top-N list.
func TestConcentrationIsAShareOfGrossAndSumsToOne(t *testing.T) {
	report := risk.Compute(risk.Input{
		Equity: d("100000"),
		Positions: []risk.Position{
			position("BTC-USDT", "BTC", "", "75000"),
			position("ETH-USDT", "ETH", "", "-25000"),
		},
	})

	total := decimal.Zero
	shares := map[string]string{}
	for _, c := range report.Portfolio.Concentration {
		total = total.Add(c.Share)
		shares[c.Asset] = c.Share.String()
	}
	require.Equal(t, "1", total.String())
	require.Equal(t, "0.75", shares["BTC"])
	require.Equal(t, "0.25", shares["ETH"],
		"a short leg contributes its size to concentration; risk has no sign")
}

// Untagged positions are their own bucket rather than being dropped: an account halfway
// through tagging its book must still see the whole of it.
func TestUntaggedPositionsAreReportedAsUngrouped(t *testing.T) {
	report := risk.Compute(risk.Input{
		Equity: d("50000"),
		Positions: []risk.Position{
			position("BTC-USDT", "BTC", "carry", "50000"),
			position("SOL-USDT", "SOL", "", "10000"),
		},
	})

	require.Equal(t, "60000", report.Portfolio.GrossExposure.String())
	ungrouped := byStrategy(t, report, "")
	require.Equal(t, "10000", ungrouped.GrossExposure.String())
	require.Equal(t, "10000", delta(t, ungrouped, "SOL").String())
}
