package binance_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// incomeRows returns the fixture's rows: [0] funding paid, [1] funding received,
// [2] TRANSFER, [3] REALIZED_PNL, [4] COMMISSION, [5] a type nobody has seen.
func incomeRows(t *testing.T) []json.RawMessage {
	t.Helper()
	var rows []json.RawMessage
	require.NoError(t, json.Unmarshal(loadFixture(t, "income_usdm.json", "payload"), &rows))
	require.Len(t, rows, 6)
	return rows
}

func normalizeIncome(t *testing.T, raw json.RawMessage) (ledger.Event, error) {
	t.Helper()
	tc := testContext()
	tc.Source = binance.SourceREST
	return binance.NormalizeIncome(context.Background(), usdmResolver(t), tc, raw)
}

// Funding is a cash flow, and its sign is the venue's. Paid is negative, received is
// positive, and the event type carries neither -- one FUNDING_PAYMENT type with a signed
// amount, rather than two types that would have to agree with each other forever.
func TestFundingBecomesASignedCashFlow(t *testing.T) {
	paid, err := normalizeIncome(t, incomeRows(t)[0])
	require.NoError(t, err)

	require.Equal(t, ledger.TypeFundingPayment, paid.EventType)
	require.Equal(t, "usdm:income:FUNDING_FEE:9689322392", paid.VenueEventID)
	require.NotNil(t, paid.InstrumentID)
	require.Equal(t, usdmBTCUSDT, *paid.InstrumentID)
	require.True(t, paid.Quantity.Decimal.Equal(decimal.RequireFromString("-0.375")))
	require.Equal(t, time.UnixMilli(1757404800000).UTC(), paid.EventTime.UTC())

	// No price, no side. Funding is not a fill: giving it a price would hand it a cost
	// basis, and giving it a side would make it look like one (K18).
	require.False(t, paid.Price.Valid)
	require.Empty(t, paid.Side)
	require.False(t, paid.Fee.Valid)

	received, err := normalizeIncome(t, incomeRows(t)[1])
	require.NoError(t, err)
	require.True(t, received.Quantity.Decimal.Equal(decimal.RequireFromString("1.25")),
		"funding received must stay positive; a normalizer that took the absolute value "+
			"would turn every payment into a cost")
}

// The instrument is resolved in the USD-M market for the same reason a fill is: funding on
// perp BTCUSDT belongs to the perp, and attaching it to the spot pair moves a cost onto a
// position that never paid it.
func TestFundingResolvesInTheFuturesMarket(t *testing.T) {
	r := usdmResolver(t)
	_, err := binance.NormalizeIncome(context.Background(), r, testContext(), incomeRows(t)[0])
	require.NoError(t, err)

	require.NotEmpty(t, r.markets)
	require.Equal(t, instrument.MarketUSDM, r.markets[0])
}

// F12, as a test.
//
// A spot to USD-M transfer appears BOTH as a MAIN_UMFUTURE row in the wallet endpoint and
// as an incomeType TRANSFER row here. M3.5 ingests the first. Folding this one as well
// moves the money twice -- and because an internal transfer folds to no delta, the second
// copy would not even show up as a doubled balance: it would show up as a withdrawal from
// the futures wallet that never happened.
//
// Skipped with a named sentinel rather than silently, because "we chose not to" and "we
// forgot" must not look the same in a log.
func TestATransferInIncomeIsSkippedBecauseTheWalletEndpointAlreadyReportedIt(t *testing.T) {
	_, err := normalizeIncome(t, incomeRows(t)[2])
	require.ErrorIs(t, err, binance.ErrIncomeReportedElsewhere)
}

// REALIZED_PNL is the venue's copy of a number this project computes (K5), and COMMISSION is
// the fee that already rides on its own fill (L9). Folding either would double it -- and the
// commission case is the quieter of the two, because a doubled fee looks like a slightly
// worse fill rather than like a bug.
func TestTheIncomeRowsThatWouldDoubleSomethingAreSkipped(t *testing.T) {
	for _, i := range []int{3, 4} {
		_, err := normalizeIncome(t, incomeRows(t)[i])
		require.ErrorIs(t, err, binance.ErrIncomeReportedElsewhere, "row %d", i)
	}
}

// The enum cannot be read off the page in full: it lists eight values and refers to fifteen
// more it does not display (F16). So an unrecognized type stops the walk rather than passing
// through as zero -- an unknown cash flow silently folded as nothing is a balance that
// drifts from the exchange's by exactly the amount nobody looked at.
func TestAnUnknownIncomeTypeIsRefused(t *testing.T) {
	_, err := normalizeIncome(t, incomeRows(t)[5])
	require.ErrorIs(t, err, binance.ErrUnknownIncomeType)
}

// The identity carries the incomeType, because the venue says so in its own words: "trandId
// is unique in the same incomeType for a user" (F3, F16). An identity built from tranId
// alone would let a funding payment and a commission with the same number collapse into one
// event, and the survivor would be whichever was walked first.
func TestTheIncomeIdentityCarriesItsType(t *testing.T) {
	require.Equal(t, "usdm:income:FUNDING_FEE:42", binance.IncomeID("FUNDING_FEE", 42))
	require.NotEqual(t, binance.IncomeID("FUNDING_FEE", 42), binance.IncomeID("COMMISSION", 42))
}

// A funding row that names no symbol cannot be attached to a position, and this fold has
// nowhere else to put it: FUNDING_PAYMENT's amount is in the settle asset OF AN INSTRUMENT.
// Refused rather than stored against nothing.
func TestFundingWithNoSymbolIsRefused(t *testing.T) {
	_, err := normalizeIncome(t, json.RawMessage(
		`{"symbol":"","incomeType":"FUNDING_FEE","income":"-1","asset":"USDT",`+
			`"time":1757404800000,"tranId":1}`))
	require.Error(t, err)
}

// Zero funding is a real row -- a position can be flat across a funding window -- and it is
// stored rather than skipped. Skipping it would leave a gap in the funding history that
// looks identical to a missing event, which is the one thing this ledger must never do.
func TestZeroFundingIsStoredRatherThanSkipped(t *testing.T) {
	event, err := normalizeIncome(t, json.RawMessage(
		`{"symbol":"BTCUSDT","incomeType":"FUNDING_FEE","income":"0","asset":"USDT",`+
			`"time":1757404800000,"tranId":7}`))
	require.NoError(t, err)
	require.True(t, event.Quantity.Valid)
	require.True(t, event.Quantity.Decimal.IsZero())
}
