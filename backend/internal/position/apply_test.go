package position_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// These tests run without Docker, because the engine is a pure function of state and
// event (L4). Time and prices arrive as inputs; nothing here reaches a clock or a network,
// which is what makes a scenario shock and a historical reconstruction the same code path.

var epoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func amount(s string) decimal.NullDecimal {
	return decimal.NewNullDecimal(decimal.RequireFromString(s))
}

// trade builds a fill. seq drives both the venue sequence and the event time, so events
// built with increasing seq are already in canonical order.
func trade(side ledger.Side, quantity, price string, seq int64) ledger.Event {
	return ledger.Event{
		VenueEventID:  fmt.Sprintf("usdm:trade:BTCUSDT:%d", seq),
		VenueSequence: seq,
		Source:        "rest",
		EventType:     ledger.TypeTrade,
		Side:          side,
		Quantity:      amount(quantity),
		Price:         amount(price),
		EventTime:     epoch.Add(time.Duration(seq) * time.Second),
	}
}

func withFee(e ledger.Event, fee, asset string) ledger.Event {
	e.Fee = amount(fee)
	e.FeeAsset = asset
	return e
}

func fold(t *testing.T, events ...ledger.Event) position.State {
	t.Helper()
	state := position.State{}
	for i, e := range events {
		var err error
		state, err = position.Apply(state, e)
		require.NoError(t, err, "event %d (%s)", i, e.VenueEventID)
	}
	return state
}

// requireState compares through String() rather than through the decimal values, so a
// failure prints the number a human can check by hand.
func requireState(t *testing.T, s position.State, quantity, avgEntry, realized string) {
	t.Helper()
	require.Equal(t, quantity, s.Quantity.String(), "quantity")
	require.Equal(t, avgEntry, s.AvgEntryPrice.String(), "average entry price")
	require.Equal(t, realized, s.RealizedPnL.String(), "realized pnl")
}

func TestTwoBuysGiveTheQuantityWeightedAverageEntry(t *testing.T) {
	// (1 x 100 + 3 x 200) / 4 = 175. Not (100 + 200) / 2 = 150, which is the mistake that
	// looks right until the position sizes differ.
	state := fold(t,
		trade(ledger.SideBuy, "1", "100", 1),
		trade(ledger.SideBuy, "3", "200", 2),
	)
	requireState(t, state, "4", "175", "0")
}

// K5: a partial close realizes on the quantity sold and leaves the entry price of what is
// still open exactly where it was. Moving it would quietly restate the cost basis of a
// position the trader has not touched.
func TestAPartialSellRealizesOnTheSoldQuantityAndLeavesTheEntryAlone(t *testing.T) {
	state := fold(t,
		trade(ledger.SideBuy, "4", "100", 1),
		trade(ledger.SideSell, "1", "150", 2),
	)
	requireState(t, state, "3", "100", "50")
}

// Closing to exactly zero must leave the entry price at zero, not at the last trade price.
// A flat position carrying an entry price reopens with a cost basis nobody paid.
func TestClosingToZeroLeavesTheAverageEntryAtZero(t *testing.T) {
	state := fold(t,
		trade(ledger.SideBuy, "1", "100", 1),
		trade(ledger.SideSell, "1", "150", 2),
	)
	requireState(t, state, "0", "0", "50")
}

// K5, and the case a naive implementation gets wrong: a sell larger than the position
// closes what is open, realizes on that quantity ONLY, and opens the remainder in the new
// direction at the trade price. Realizing on the full sell quantity would invent PnL on
// a position that never existed.
func TestASellLargerThanThePositionFlipsIt(t *testing.T) {
	state := fold(t,
		trade(ledger.SideBuy, "1", "100", 1),
		trade(ledger.SideSell, "3", "150", 2),
	)
	// Closed 1 at 150 against an entry of 100 -> 50. The remaining 2 open short at 150.
	requireState(t, state, "-2", "150", "50")
}

func TestAddingToAShortAveragesTheEntry(t *testing.T) {
	state := fold(t,
		trade(ledger.SideSell, "1", "100", 1),
		trade(ledger.SideSell, "1", "200", 2),
	)
	requireState(t, state, "-2", "150", "0")
}

// A short earns when the price falls, so the sign of realized PnL is the mirror of the
// long case. Getting this backwards is invisible in a long-only test suite.
func TestAShortRealizesInTheOppositeDirection(t *testing.T) {
	state := fold(t,
		trade(ledger.SideSell, "2", "100", 1),
		trade(ledger.SideBuy, "1", "80", 2),
	)
	requireState(t, state, "-1", "100", "20")
}

func TestABuyLargerThanAShortFlipsItLong(t *testing.T) {
	state := fold(t,
		trade(ledger.SideSell, "2", "100", 1),
		trade(ledger.SideBuy, "5", "90", 2),
	)
	// Closed 2 at 90 against an entry of 100 -> 20. The remaining 3 open long at 90.
	requireState(t, state, "3", "90", "20")
}

// L9, K18: THE TEST THIS CASE EXISTS FOR. A fee is never folded into the average entry
// price -- doing so hides a cost inside the cost basis, where it silently changes every
// unrealized PnL number afterwards. And a fee paid in BNB cannot be subtracted from a
// USDT-denominated realized PnL, because converting it needs a price this engine does not
// have and must not invent (that missing price is what fee_price_missing reports, K23).
func TestAFeeNeverTouchesTheAverageEntryPriceOrTheQuotePnL(t *testing.T) {
	state := fold(t,
		withFee(trade(ledger.SideBuy, "1", "100", 1), "0.000375", "BNB"),
		withFee(trade(ledger.SideBuy, "1", "200", 2), "0.15", "USDT"),
	)
	requireState(t, state, "2", "150", "0")

	require.Equal(t, []position.FeeTotal{
		{Asset: "BNB", Amount: decimal.RequireFromString("0.000375")},
		{Asset: "USDT", Amount: decimal.RequireFromString("0.15")},
	}, state.Fees, "fees stay per asset, in a stable order, unconverted")
}

func TestFeesInOneAssetAccumulate(t *testing.T) {
	state := fold(t,
		withFee(trade(ledger.SideBuy, "1", "100", 1), "0.1", "USDT"),
		withFee(trade(ledger.SideSell, "1", "100", 2), "0.2", "USDT"),
	)
	require.Len(t, state.Fees, 1)
	require.Equal(t, "0.3", state.Fees[0].Amount.String())
}

// A standalone FEE event is the only fee with no parent (K18). It moves the fee totals and
// nothing else -- in particular it must not be mistaken for a zero-quantity trade.
func TestAStandaloneFeeEventMovesOnlyTheFeeTotals(t *testing.T) {
	fee := trade(ledger.SideBuy, "1", "100", 2)
	fee.EventType = ledger.TypeFee
	fee.Side = ""
	fee.Quantity = decimal.NullDecimal{}
	fee.Price = decimal.NullDecimal{}
	fee = withFee(fee, "1.25", "USDT")

	state := fold(t, trade(ledger.SideBuy, "1", "100", 1), fee)
	requireState(t, state, "1", "100", "0")
	require.Equal(t, "1.25", state.Fees[0].Amount.String())
}

// Funding is a cash flow against the position, not a change to its cost basis. V1 is
// USD-M only (PROJECT.md §1), so the settle asset is the quote asset and folding it into
// the quote-denominated realized PnL is exact rather than an approximation.
func TestFundingMovesRealizedPnLWithoutTouchingTheEntry(t *testing.T) {
	funding := trade(ledger.SideBuy, "1", "100", 2)
	funding.EventType = ledger.TypeFundingPayment
	funding.Side = ""
	funding.Price = decimal.NullDecimal{}
	funding.Quantity = amount("-0.42") // paid, not received

	state := fold(t, trade(ledger.SideBuy, "1", "100", 1), funding)
	requireState(t, state, "1", "100", "-0.42")
}

// L6/L7: the fold must not replay an event it has already seen, and the cursor it checks
// against is the whole canonical order -- not venue_sequence alone. Two events can share
// a sequence, so a bare last_venue_sequence cursor would reject the second one and drop it
// from the position for good.
func TestTheCursorRejectsAReplayButAcceptsASequenceTie(t *testing.T) {
	first := trade(ledger.SideBuy, "1", "100", 1)

	state, err := position.Apply(position.State{}, first)
	require.NoError(t, err)

	_, err = position.Apply(state, first)
	require.ErrorIs(t, err, position.ErrOutOfOrder, "the same event twice must be refused")

	earlier := trade(ledger.SideBuy, "1", "100", 0)
	_, err = position.Apply(state, earlier)
	require.ErrorIs(t, err, position.ErrOutOfOrder, "an event before the cursor must be refused")

	// Same event_time, same venue_sequence, a later venue_event_id: a second fill of one
	// order. It is genuinely next in canonical order and must be applied.
	tied := first
	tied.VenueEventID = first.VenueEventID + ":b"
	tied.Price = amount("200")

	state, err = position.Apply(state, tied)
	require.NoError(t, err, "a sequence tie broken by venue_event_id must still advance")
	requireState(t, state, "2", "150", "0")
}

func TestTheCursorAdvancesToTheEventJustApplied(t *testing.T) {
	e := trade(ledger.SideBuy, "1", "100", 7)
	state, err := position.Apply(position.State{}, e)
	require.NoError(t, err)
	require.Equal(t, e.Cursor(), state.Cursor)
}

// Events that move balances but not this position still advance the cursor, so the fold
// does not stall and does not reprocess them.
func TestABalanceEventAdvancesTheCursorAndChangesNothingElse(t *testing.T) {
	deposit := trade(ledger.SideBuy, "1", "100", 2)
	deposit.EventType = ledger.TypeDeposit
	deposit.Side = ""
	deposit.Price = decimal.NullDecimal{}

	state := fold(t, trade(ledger.SideBuy, "1", "100", 1), deposit)
	requireState(t, state, "1", "100", "0")
	require.Equal(t, deposit.Cursor(), state.Cursor)
}

// A fill without a price or a quantity is not foldable. The schema refuses to store one,
// and the engine refuses to fold one, because these are two different doors into the same
// state and only one of them is Postgres.
func TestAFillMissingItsPriceOrQuantityIsRejected(t *testing.T) {
	for name, mutate := range map[string]func(*ledger.Event){
		"no price":    func(e *ledger.Event) { e.Price = decimal.NullDecimal{} },
		"no quantity": func(e *ledger.Event) { e.Quantity = decimal.NullDecimal{} },
		"no side":     func(e *ledger.Event) { e.Side = "" },
		"zero quantity": func(e *ledger.Event) {
			e.Quantity = amount("0")
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := trade(ledger.SideBuy, "1", "100", 1)
			mutate(&e)
			_, err := position.Apply(position.State{}, e)
			require.ErrorIs(t, err, position.ErrMalformedEvent)
		})
	}
}

// An event type the engine cannot fold is an error, never a silent skip. A hole in the
// fold shows up months later as a position that is wrong by exactly one trade.
func TestAnEventTypeTheEngineCannotFoldIsAnError(t *testing.T) {
	adjustment := trade(ledger.SideBuy, "1", "100", 1)
	adjustment.EventType = ledger.TypePositionAdjustment

	_, err := position.Apply(position.State{}, adjustment)
	require.ErrorIs(t, err, position.ErrUnsupportedEventType)
	require.ErrorContains(t, err, "POSITION_ADJUSTMENT")
}

// A liquidation is a fill the exchange chose for you, and folds exactly like one (K6).
func TestALiquidationFoldsLikeATrade(t *testing.T) {
	liquidation := trade(ledger.SideSell, "1", "60", 2)
	liquidation.EventType = ledger.TypeLiquidation

	state := fold(t, trade(ledger.SideBuy, "1", "100", 1), liquidation)
	requireState(t, state, "0", "0", "-40")
}

// Apply is a pure function, so the state handed to it must come back untouched -- fees
// included. The Fees slice is the one field that could alias its way into a shared backing
// array, which would make a rebuild depend on who folded first.
func TestApplyDoesNotMutateTheStateItWasGiven(t *testing.T) {
	base := fold(t, withFee(trade(ledger.SideBuy, "1", "100", 1), "0.1", "USDT"))

	_, err := position.Apply(base, withFee(trade(ledger.SideBuy, "1", "300", 2), "0.9", "USDT"))
	require.NoError(t, err)

	requireState(t, base, "1", "100", "0")
	require.Equal(t, "0.1", base.Fees[0].Amount.String(), "the caller's fees were mutated")
}

// L3 depends on the fold being reproducible after a round trip through
// NUMERIC(38,18). A realized-PnL term can carry more than 18 decimals -- the entry price
// already has exactly 18, and multiplying by a fractional quantity doubles that -- so
// without rounding, an incremental fold that reloaded a stored value and a rebuild that
// never stored anything accumulate different numbers and the projection cannot be rebuilt.
//
// This case uses a price that does not divide evenly, which is why the whole-number
// fixtures elsewhere in this file never caught it.
func TestRealizedPnLSurvivesARoundTripThroughTheStoredScale(t *testing.T) {
	buys := []ledger.Event{
		trade(ledger.SideBuy, "1", "100", 1),
		trade(ledger.SideBuy, "2", "200", 2),
	}
	firstSell := trade(ledger.SideSell, "0.5", "200", 3)
	secondSell := trade(ledger.SideSell, "0.5", "200", 4)

	require.Equal(t, "166.666666666666666667",
		fold(t, buys...).AvgEntryPrice.String(), "the entry price is already at full scale")

	// One continuous fold: what Rebuild does.
	rebuilt := fold(t, append(append([]ledger.Event{}, buys...), firstSell, secondSell)...)

	// The same fold interrupted by a store and a reload: what Project does. Postgres
	// rounds to scale 18 on write, so the reloaded state carries the rounded value.
	stored := fold(t, append(append([]ledger.Event{}, buys...), firstSell)...)
	stored.Quantity = stored.Quantity.Round(18)
	stored.AvgEntryPrice = stored.AvgEntryPrice.Round(18)
	stored.RealizedPnL = stored.RealizedPnL.Round(18)

	incremental, err := position.Apply(stored, secondSell)
	require.NoError(t, err)

	require.Equal(t, rebuilt.RealizedPnL.String(), incremental.RealizedPnL.String(),
		"an incremental fold and a rebuild produced different realized PnL")
}
