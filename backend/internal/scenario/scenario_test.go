package scenario_test

import (
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/Contictus/plimsoll/backend/internal/scenario"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// oneTier is a bracket table flat enough that nothing crosses a tier, so a test about
// something else is not also a test about tiers.
func oneTier() []collateral.Bracket {
	return []collateral.Bracket{{
		Bracket:          1,
		NotionalFloor:    dec("0"),
		NotionalCap:      dec("100000000"),
		MaintMarginRatio: dec("0.01"),
		Cum:              dec("0"),
	}}
}

// longPerp is a BTC position at 50k, marked at 50k, so it starts with no unrealized PnL and
// every change below is attributable to the shock.
func longPerp(qty string) scenario.Position {
	return scenario.Position{
		Symbol:     "BTCUSDT",
		BaseAsset:  "BTC",
		Quantity:   dec(qty),
		EntryPrice: dec("50000"),
		MarkPrice:  dec("50000"),
		Brackets:   oneTier(),
	}
}

func base() scenario.Input {
	return scenario.Input{
		WalletBalance: dec("10000"),
		Positions:     []scenario.Position{longPerp("1")},
	}
}

func shock(asset, move string) map[string]decimal.Decimal {
	return map[string]decimal.Decimal{asset: dec(move)}
}

// A shock moves the asset it names and nothing else. Inventing a correlation produces a
// number that looks like analysis and is a guess (K56).
func TestAShockMovesOnlyTheAssetItNames(t *testing.T) {
	in := base()
	in.Positions = append(in.Positions, scenario.Position{
		Symbol: "ETHUSDT", BaseAsset: "ETH",
		Quantity: dec("10"), EntryPrice: dec("3000"), MarkPrice: dec("3000"),
		Brackets: oneTier(),
	})
	in.Shocks = shock("BTC", "-0.2")

	got, err := scenario.Project(in)
	require.NoError(t, err)

	// Only the BTC leg moved: 1 BTC * 50000 * -0.2 = -10000.
	require.Equal(t, "-10000", got.Shocked.UnrealizedPnL.String())
}

// THE TEST THIS MILESTONE EXISTS FOR.
//
// Long spot BTC and short BTC perp is a hedge. A scenario that moved only one leg would report
// a loss the user does not have -- the false alarm K13 prevents for leverage, arriving here
// through a different door. Both legs are the same asset and move together (K56).
func TestAHedgedBookLosesNothingToAShock(t *testing.T) {
	in := scenario.Input{
		WalletBalance: dec("10000"),
		Positions:     []scenario.Position{longPerp("-1")},
		Holdings: []scenario.Holding{{
			Asset:    "BTC",
			Quantity: dec("1"),
			Price:    decimal.NewNullDecimal(dec("50000")),
		}},
		Shocks: shock("BTC", "-0.2"),
	}

	got, err := scenario.Project(in)
	require.NoError(t, err)

	require.Equal(t, got.Base.Equity.String(), got.Shocked.Equity.String(),
		"the perp gains exactly what the spot loses; the book is flat")
}

// ...and the same book without its spot leg does lose, so the test above is not passing for
// some reason unrelated to hedging.
func TestTheSameBookWithoutItsSpotLegDoesMove(t *testing.T) {
	in := scenario.Input{
		WalletBalance: dec("10000"),
		Positions:     []scenario.Position{longPerp("-1")},
		Shocks:        shock("BTC", "-0.2"),
	}

	got, err := scenario.Project(in)
	require.NoError(t, err)
	require.Equal(t, "10000", got.Shocked.Equity.Sub(got.Base.Equity).String(),
		"a naked short gains when the price falls")
}

// Maintenance is recomputed from the tier table at the shocked notional, never scaled from
// today's figure. A shock big enough to model is usually big enough to cross a tier, and
// scaling assumes a constant rate -- wrong in the direction that understates the danger (F15).
func TestMaintenanceIsRecomputedAcrossATierAndNotScaled(t *testing.T) {
	tiers := []collateral.Bracket{
		{Bracket: 1, NotionalFloor: dec("0"), NotionalCap: dec("50000"),
			MaintMarginRatio: dec("0.01"), Cum: dec("0")},
		{Bracket: 2, NotionalFloor: dec("50000"), NotionalCap: dec("1000000"),
			MaintMarginRatio: dec("0.05"), Cum: dec("2000")},
	}
	in := scenario.Input{
		WalletBalance: dec("100000"),
		Positions: []scenario.Position{{
			Symbol: "BTCUSDT", BaseAsset: "BTC",
			Quantity: dec("1"), EntryPrice: dec("40000"), MarkPrice: dec("40000"),
			Brackets: tiers,
		}},
		Shocks: shock("BTC", "0.5"),
	}

	got, err := scenario.Project(in)
	require.NoError(t, err)

	// Base: notional 40000 in tier 1 -> 40000*0.01 - 0 = 400.
	require.Equal(t, "400", got.Base.MaintenanceMargin.Decimal.String())
	// Shocked: notional 60000 in tier 2 -> 60000*0.05 - 2000 = 1000.
	// Scaling the base by 1.5 would have said 600, understating it by forty percent.
	require.Equal(t, "1000", got.Shocked.MaintenanceMargin.Decimal.String())
}

// A bracket table we never captured makes the buffer UNAVAILABLE, not smaller.
//
// Summing only the requirements we happen to know understates the requirement, which overstates
// the buffer -- reporting the account as safer than it is, at the exact moment it is being
// asked whether it is safe (K56).
func TestAMissingBracketTableMakesTheBufferUnavailableNotLarger(t *testing.T) {
	in := base()
	in.Positions = append(in.Positions, scenario.Position{
		Symbol: "ETHUSDT", BaseAsset: "ETH",
		Quantity: dec("10"), EntryPrice: dec("3000"), MarkPrice: dec("3000"),
		Brackets: nil,
	})
	in.Shocks = shock("BTC", "-0.2")

	got, err := scenario.Project(in)
	require.NoError(t, err)

	require.False(t, got.Shocked.Buffer.Valid, "an incomplete requirement is not a buffer")
	require.False(t, got.Shocked.MaintenanceMargin.Valid)
	require.Contains(t, got.Unavailable, "ETHUSDT")
	require.False(t, got.Shocked.Liquidated,
		"a buffer nobody could compute must not be reported as a liquidation either way")
}

// An unpriced holding IS excluded from equity, because that understates equity and therefore
// overstates the danger -- the safe direction. It is named all the same (L11, K56).
func TestAnUnpricedHoldingIsExcludedAndNamed(t *testing.T) {
	in := base()
	in.Holdings = []scenario.Holding{
		{Asset: "BTC", Quantity: dec("1"), Price: decimal.NewNullDecimal(dec("50000"))},
		{Asset: "WTF", Quantity: dec("999")},
	}
	in.Shocks = shock("BTC", "-0.2")

	got, err := scenario.Project(in)
	require.NoError(t, err)

	require.Contains(t, got.Unavailable, "WTF")
	require.True(t, got.Shocked.Buffer.Valid,
		"an unpriced holding is conservative, so it does not invalidate the buffer")
	require.Equal(t, "60000", got.Base.Equity.String(),
		"10000 wallet plus 50000 of spot; none of the 999 WTF is counted")
}

// A price cannot go below zero. A move at or past -1 is refused rather than clamped: clamping
// answers a question the user did not ask and presents it as the one they did.
func TestAMoveThatTakesThePriceToZeroOrBelowIsRefused(t *testing.T) {
	for _, move := range []string{"-1", "-1.5", "-2"} {
		in := base()
		in.Shocks = shock("BTC", move)
		_, err := scenario.Project(in)
		require.Error(t, err, "move %s", move)
	}
}

// A shock naming an asset the account does not hold is not an error -- it is a scenario that
// happens not to touch this book, and refusing it would break a saved scenario the moment a
// position is closed.
func TestAShockOnAnAssetTheAccountDoesNotHoldIsHarmless(t *testing.T) {
	in := base()
	in.Shocks = shock("DOGE", "-0.9")

	got, err := scenario.Project(in)
	require.NoError(t, err)
	require.Equal(t, got.Base.Equity.String(), got.Shocked.Equity.String())
}

// The liquidation flag is why the buffer is signed: past the line is a state, not a zero.
func TestAShockDeepEnoughToPassTheLineSaysSo(t *testing.T) {
	in := scenario.Input{
		WalletBalance: dec("5000"),
		Positions:     []scenario.Position{longPerp("10")},
		Shocks:        shock("BTC", "-0.05"),
	}

	got, err := scenario.Project(in)
	require.NoError(t, err)
	require.False(t, got.Base.Liquidated, "it is solvent before the shock")
	require.True(t, got.Shocked.Liquidated)
	require.True(t, got.Shocked.Buffer.Decimal.IsNegative(),
		"how far past the line is the whole question, so it is never clamped at zero")
}

// L4: pure. The same input twice gives the same projection, and Project reads no clock.
func TestProjectIsPure(t *testing.T) {
	in := base()
	in.Shocks = shock("BTC", "-0.2")

	first, err := scenario.Project(in)
	require.NoError(t, err)
	second, err := scenario.Project(in)
	require.NoError(t, err)
	require.Equal(t, first, second)
}
