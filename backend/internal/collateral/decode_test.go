package collateral_test

import (
	"encoding/json"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/stretchr/testify/require"
)

const accountPayload = `{
  "totalMaintMargin": "1500.00",
  "totalWalletBalance": "10000.00",
  "totalUnrealizedProfit": "250.00",
  "totalMarginBalance": "10250.00",
  "availableBalance": "8000.00",
  "assets": [], "positions": []
}`

const positionsPayload = `[
  {"symbol":"BTCUSDT","positionAmt":"0.500","entryPrice":"60000","markPrice":"61000",
   "liquidationPrice":"48000","notional":"30500","leverage":"5","positionSide":"BOTH"},
  {"symbol":"ETHUSDT","positionAmt":"0","entryPrice":"0","markPrice":"3000",
   "liquidationPrice":"0","notional":"0","leverage":"10","positionSide":"BOTH"}
]`

// The account endpoint is where the maintenance requirement lives (F14), and the buffer is
// built from two of its fields. Read as strings all the way to decimal: a margin balance
// through float64 comes back with different digits (L1).
func TestTheAccountTotalsAreReadAsDecimals(t *testing.T) {
	s, err := collateral.DecodeAccount(json.RawMessage(accountPayload))
	require.NoError(t, err)

	require.Equal(t, "10250", s.MarginBalance.String())
	require.Equal(t, "1500", s.MaintenanceMargin.String())
	require.Equal(t, "10000", s.WalletBalance.String())
	require.Equal(t, "250", s.UnrealizedPnL.String())
	require.Equal(t, "8000", s.AvailableBalance.String())

	require.Equal(t, "8750", collateral.Buffer(s).String())
}

// F14's open question, answered in the safe direction: the endpoint may return a row for
// every symbol, so a zero positionAmt is NO POSITION rather than a position of zero.
//
// Storing the flat ones would fill the risk view with hundreds of rows at zero, each with a
// liquidation distance of nothing, and bury the one position that matters.
func TestAFlatRowIsNotAPosition(t *testing.T) {
	positions, err := collateral.DecodePositions(json.RawMessage(positionsPayload))
	require.NoError(t, err)

	require.Len(t, positions, 1, "a flat row was stored as a position")
	require.Equal(t, "BTCUSDT", positions[0].Symbol)
	require.Equal(t, "0.5", positions[0].Quantity.String())
	require.Equal(t, "48000", positions[0].LiquidationPrice.String())
	require.Equal(t, "61000", positions[0].MarkPrice.String())
}

// A hedge-mode row here is the same refusal the fill normalizer makes, for the same reason:
// one position per instrument, and two sides folded into it report a flat account that is
// carrying two live exposures.
func TestAHedgeModePositionIsRefused(t *testing.T) {
	_, err := collateral.DecodePositions(json.RawMessage(
		`[{"symbol":"BTCUSDT","positionAmt":"1","positionSide":"LONG","markPrice":"1",
		   "entryPrice":"1","liquidationPrice":"0","notional":"1","leverage":"1"}]`))
	require.ErrorIs(t, err, collateral.ErrHedgeMode)
}

// The tier table arrives per symbol, and the brackets must come back in the order the
// half-open lookup depends on. A table sorted by anything but its floor would make
// MaintenanceAt match the wrong tier for a notional sitting near a boundary.
func TestBracketsComeBackOrderedByTheirFloor(t *testing.T) {
	table, err := collateral.DecodeBrackets(json.RawMessage(`[
	  {"symbol":"BTCUSDT","brackets":[
	    {"bracket":2,"notionalFloor":50000,"notionalCap":250000,"maintMarginRatio":0.005,"cum":50.0},
	    {"bracket":1,"notionalFloor":0,"notionalCap":50000,"maintMarginRatio":0.004,"cum":0.0}]}]`))
	require.NoError(t, err)

	got := table["BTCUSDT"]
	require.Len(t, got, 2)
	require.Equal(t, "0", got[0].NotionalFloor.String())
	require.Equal(t, "50000", got[1].NotionalFloor.String())

	// And the decoded table answers the same way the hand-built one does.
	maint, err := collateral.MaintenanceAt(got, d("100000"))
	require.NoError(t, err)
	require.Equal(t, "450", maint.String())
}

// The bracket endpoint sends its numbers as JSON NUMBERS, not strings -- unlike every other
// money field on this venue -- so the decoder has to choose how to carry them.
//
// The obvious wrong choice, float64, hides well: decimal.NewFromFloat(0.005) prints "0.005",
// because it recovers the shortest representation that round-trips. It only breaks past
// float64's ~17 significant digits, which is why this test uses a value with more of them
// rather than a plausible-looking rate. A test written with 0.005 would have asserted
// nothing at all -- and this one was, until the mutation that should have killed it did not.
func TestBracketRatesSurviveDecodingExactly(t *testing.T) {
	const exact = "0.00123456789012345678"
	table, err := collateral.DecodeBrackets(json.RawMessage(`[
	  {"symbol":"BTCUSDT","brackets":[
	    {"bracket":1,"notionalFloor":0,"notionalCap":50000,
	     "maintMarginRatio":` + exact + `,"cum":0.0}]}]`))
	require.NoError(t, err)
	require.Equal(t, exact, table["BTCUSDT"][0].MaintMarginRatio.String(),
		"the rate lost digits on the way in; float64 keeps about seventeen")
}
