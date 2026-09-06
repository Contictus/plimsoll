package portfolio_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

var (
	buildAsOf = time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	oneAcct   = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	twoAcct   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func holding(
	integration uuid.UUID, instrument int64, symbol, quote, qty, avg, realized string,
) portfolio.Position {
	return portfolio.Position{
		IntegrationID: integration,
		InstrumentID:  instrument,
		Symbol:        symbol,
		Kind:          "spot",
		BaseAsset:     "BASE",
		QuoteAsset:    quote,
		Quantity:      dec(qty),
		AvgEntryPrice: dec(avg),
		RealizedPnL:   dec(realized),
	}
}

// Cost basis is what was paid to hold this, and a short paid something too. Signing it
// would make the portfolio's cost basis shrink as the account took on more risk.
func TestCostBasisIsUnsignedAndDerivedFromQuantityAndEntry(t *testing.T) {
	long := holding(oneAcct, 1, "BTC-USDT", "USDT", "0.5", "60000", "0")
	short := holding(oneAcct, 2, "ETH-USDT", "USDT", "-3", "2500", "0")

	got := portfolio.Build(portfolio.Input{AsOf: buildAsOf, Positions: []portfolio.Position{long, short}})

	require.Equal(t, "30000", got.Holdings[0].CostBasis.String())
	require.Equal(t, "7500", got.Holdings[1].CostBasis.String(),
		"a short position has a cost basis too, and it is not negative")
}

// THE TEST THIS ENGINE EXISTS FOR.
//
// Realized PnL on BTC-USDT is denominated in USDT and on ETH-BTC in BTC. Adding them
// produces a number with no unit, which is the "every screen shows a different total"
// failure in miniature (K11, L10). Until a price source exists there is no total, only
// subtotals -- and they are per quote asset.
func TestTotalsAreNeverSummedAcrossQuoteAssets(t *testing.T) {
	usdt := holding(oneAcct, 1, "BTC-USDT", "USDT", "1", "60000", "1200.5")
	btc := holding(oneAcct, 2, "ETH-BTC", "BTC", "10", "0.05", "0.004")

	got := portfolio.Build(portfolio.Input{AsOf: buildAsOf, Positions: []portfolio.Position{usdt, btc}})

	require.Len(t, got.ByQuote, 2)
	require.Equal(t, "BTC", got.ByQuote[0].Asset, "subtotals are ordered by asset, so a diff is stable")
	require.Equal(t, "0.5", got.ByQuote[0].CostBasis.String())
	require.Equal(t, "0.004", got.ByQuote[0].RealizedPnL.String())
	require.Equal(t, "USDT", got.ByQuote[1].Asset)
	require.Equal(t, "60000", got.ByQuote[1].CostBasis.String())
	require.Equal(t, "1200.5", got.ByQuote[1].RealizedPnL.String())
}

// Two integrations trading the same pair in the same quote are one subtotal, because a
// portfolio is the account's, not the connection's.
func TestSubtotalsSpanIntegrations(t *testing.T) {
	a := holding(oneAcct, 1, "BTC-USDT", "USDT", "1", "60000", "100")
	b := holding(twoAcct, 1, "BTC-USDT", "USDT", "2", "50000", "-40")

	got := portfolio.Build(portfolio.Input{AsOf: buildAsOf, Positions: []portfolio.Position{a, b}})

	require.Len(t, got.ByQuote, 1)
	require.Equal(t, "160000", got.ByQuote[0].CostBasis.String())
	require.Equal(t, "60", got.ByQuote[0].RealizedPnL.String())
}

// Fees stay per asset and unconverted (K18, L9): a fee in BNB against a USDT-denominated
// position needs a price, and this engine has none.
func TestFeesAreTotalledPerAssetAndLeftUnconverted(t *testing.T) {
	a := holding(oneAcct, 1, "BTC-USDT", "USDT", "1", "60000", "0")
	a.Fees = []position.FeeTotal{{Asset: "BNB", Amount: dec("0.01")}, {Asset: "USDT", Amount: dec("3")}}
	b := holding(oneAcct, 2, "ETH-USDT", "USDT", "1", "2500", "0")
	b.Fees = []position.FeeTotal{{Asset: "BNB", Amount: dec("0.02")}}

	got := portfolio.Build(portfolio.Input{AsOf: buildAsOf, Positions: []portfolio.Position{a, b}})

	require.Equal(t, []position.FeeTotal{
		{Asset: "BNB", Amount: dec("0.03")},
		{Asset: "USDT", Amount: dec("3")},
	}, got.Fees)
}

// A flat position is history, not a holding: it has realized PnL and fees that belong in
// the totals, and no quantity to show. Dropping it would lose the realized PnL; keeping it
// in Holdings would fill a dashboard with rows the user closed months ago -- so it stays,
// flagged, and the client decides.
func TestAFlatPositionIsMarkedRatherThanDropped(t *testing.T) {
	flat := holding(oneAcct, 1, "BTC-USDT", "USDT", "0", "0", "900")

	got := portfolio.Build(portfolio.Input{AsOf: buildAsOf, Positions: []portfolio.Position{flat}})

	require.Len(t, got.Holdings, 1)
	require.True(t, got.Holdings[0].Flat)
	require.Equal(t, "900", got.ByQuote[0].RealizedPnL.String(),
		"a closed position's realized PnL is still the account's")
}

// Empty must serialize as [] and not null: a client iterating the field should not have to
// nil-check it, the same reason freshness.Report always carries a list.
func TestAnEmptyPortfolioCarriesListsRatherThanNulls(t *testing.T) {
	got := portfolio.Build(portfolio.Input{AsOf: buildAsOf})

	require.NotNil(t, got.Holdings)
	require.NotNil(t, got.ByQuote)
	require.NotNil(t, got.Fees)
	require.Empty(t, got.Holdings)
	require.Equal(t, buildAsOf, got.AsOf)
}

// Build must not depend on the order it was handed rows: the same set in any order is the
// same portfolio, byte for byte, or a diff between two reads is meaningless.
func TestBuildIsOrderIndependent(t *testing.T) {
	a := holding(oneAcct, 2, "ETH-USDT", "USDT", "1", "2500", "0")
	b := holding(twoAcct, 1, "BTC-USDT", "USDT", "1", "60000", "0")
	c := holding(oneAcct, 1, "BTC-USDT", "USDT", "1", "59000", "0")

	forward := portfolio.Build(portfolio.Input{AsOf: buildAsOf, Positions: []portfolio.Position{a, b, c}})
	backward := portfolio.Build(portfolio.Input{AsOf: buildAsOf, Positions: []portfolio.Position{c, b, a}})

	require.Equal(t, forward, backward)
}
