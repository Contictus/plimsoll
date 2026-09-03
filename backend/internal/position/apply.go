package position

import (
	"errors"
	"fmt"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"
)

// The three ways an event can fail to fold. Each is an error rather than a skip: a hole in
// the fold surfaces months later as a position that is wrong by exactly one trade, with
// nothing in the logs to say when it happened.
var (
	// ErrOutOfOrder means the event is not strictly after the state's cursor -- a replay,
	// or a fold reading in something other than canonical order.
	ErrOutOfOrder = errors.New("position: event is not after the current cursor")

	// ErrMalformedEvent means the event claims to be a fill but is missing something a
	// fill cannot be computed without.
	ErrMalformedEvent = errors.New("position: event cannot be folded as written")

	// ErrUnsupportedEventType means the engine has no rule for this type yet. Better a
	// halted projection than a quietly incomplete one.
	ErrUnsupportedEventType = errors.New("position: no fold rule for this event type")
)

// Apply folds one event into a position and returns the new state. It never modifies the
// state it is given.
//
// Events must arrive in canonical order (event_time, venue_sequence, venue_event_id);
// anything at or before the cursor is refused, so a replayed backfill cannot double a
// position (L6, L7). Every event that is accepted advances the cursor, including those
// that leave the position itself untouched.
func Apply(s State, e ledger.Event) (State, error) {
	if !after(e.Cursor(), s.Cursor) {
		return s, fmt.Errorf("%w: %s at %s is not after %s",
			ErrOutOfOrder, e.VenueEventID, e.EventTime, s.Cursor.EventTime)
	}

	next, err := foldByType(s, e)
	if err != nil {
		return s, err
	}

	// The fee rides on the event that caused it and is never folded into the entry price
	// (K18, L9). Applied after the type-specific fold so that it lands the same way on a
	// trade, a funding payment and a standalone FEE event.
	if e.Fee.Valid && !e.Fee.Decimal.IsZero() {
		if e.FeeAsset == "" {
			return s, fmt.Errorf("%w: %s carries a fee with no asset",
				ErrMalformedEvent, e.VenueEventID)
		}
		next.Fees = addFee(next.Fees, e.FeeAsset, e.Fee.Decimal)
	}

	next.Cursor = e.Cursor()
	return next, nil
}

func foldByType(s State, e ledger.Event) (State, error) {
	switch e.EventType {
	case ledger.TypeTrade, ledger.TypeLiquidation:
		// A liquidation is a fill the exchange chose for you; the arithmetic is the same
		// one, and only the flag distinguishes them downstream (K6).
		return applyFill(s, e)

	case ledger.TypeFundingPayment:
		// A cash flow against the position, not a change to its cost basis. V1 is USD-M
		// only (PROJECT.md §1), so the settle asset is the quote asset and this addition
		// is exact rather than an approximation. COIN-M would need a price and therefore
		// belongs in valuation, not here.
		if !e.Quantity.Valid {
			return s, fmt.Errorf("%w: %s is a funding payment with no amount",
				ErrMalformedEvent, e.VenueEventID)
		}
		s.RealizedPnL = s.RealizedPnL.Add(e.Quantity.Decimal)
		return s, nil

	case ledger.TypeFee, ledger.TypeCommissionRebate:
		// Both move only the fee totals, which Apply handles for every event type. A
		// rebate arrives as a negative fee rather than as a separate field, so the two
		// never have to be added together later.
		return s, nil

	case ledger.TypeDeposit, ledger.TypeWithdrawal, ledger.TypeTransfer:
		// Balance events. They move what the account holds, not what a position costs, so
		// they advance the cursor and change nothing here.
		return s, nil

	default:
		return s, fmt.Errorf("%w: %s (%s)", ErrUnsupportedEventType, e.EventType, e.VenueEventID)
	}
}

// applyFill is exchange-style average cost in one-way mode (K5). Three cases, and the
// third is the one a naive implementation gets wrong.
func applyFill(s State, e ledger.Event) (State, error) {
	signed, price, err := fillTerms(e)
	if err != nil {
		return s, err
	}

	switch {
	// Opening, or adding in the direction already held: the entry price becomes the
	// quantity-weighted average of what was there and what was just bought.
	case s.Quantity.IsZero() || s.Quantity.Sign() == signed.Sign():
		held, added := s.Quantity.Abs(), signed.Abs()
		total := held.Add(added)
		s.AvgEntryPrice = held.Mul(s.AvgEntryPrice).Add(added.Mul(price)).DivRound(total, moneyScale)
		s.Quantity = s.Quantity.Add(signed)

	// Closing part or all of the position: PnL is realized on the quantity closed, and the
	// entry price of whatever is still open does not move. Restating it would change the
	// cost basis of a position the trader has not touched.
	case signed.Abs().LessThanOrEqual(s.Quantity.Abs()):
		s.RealizedPnL = s.RealizedPnL.Add(realized(s, signed.Abs(), price))
		s.Quantity = s.Quantity.Add(signed)
		if s.Quantity.IsZero() {
			// Flat means flat. An entry price left behind here would reopen the next
			// position with a cost basis nobody paid.
			s.AvgEntryPrice = decimal.Zero
		}

	// Flipping: the fill is larger than the position. PnL is realized on the closed
	// quantity ONLY -- realizing on the whole fill would invent PnL on a position that
	// never existed -- and the remainder opens in the new direction at this price.
	default:
		s.RealizedPnL = s.RealizedPnL.Add(realized(s, s.Quantity.Abs(), price))
		s.Quantity = s.Quantity.Add(signed)
		s.AvgEntryPrice = price
	}

	return s, nil
}

// realized is the PnL of closing `closed` units of the current position at `price`. A long
// earns when the price rises and a short when it falls, which is the sign the current
// quantity carries.
func realized(s State, closed, price decimal.Decimal) decimal.Decimal {
	move := price.Sub(s.AvgEntryPrice)
	if s.Quantity.Sign() < 0 {
		move = move.Neg()
	}
	return closed.Mul(move)
}

// fillTerms validates the event as a fill and returns its signed quantity and its price.
// The schema refuses to store a fill missing any of this, and the engine refuses to fold
// one: they are two different doors into the same state, and only one of them is Postgres.
func fillTerms(e ledger.Event) (signed, price decimal.Decimal, err error) {
	switch {
	case !e.Quantity.Valid || e.Quantity.Decimal.Sign() <= 0:
		return signed, price, fmt.Errorf("%w: %s is a fill without a positive quantity",
			ErrMalformedEvent, e.VenueEventID)
	case !e.Price.Valid || e.Price.Decimal.Sign() < 0:
		return signed, price, fmt.Errorf("%w: %s is a fill without a price",
			ErrMalformedEvent, e.VenueEventID)
	}

	// Direction lives on side and magnitude on quantity, so a fill can never carry the
	// direction twice and disagree with itself.
	switch e.Side {
	case ledger.SideBuy:
		return e.Quantity.Decimal, e.Price.Decimal, nil
	case ledger.SideSell:
		return e.Quantity.Decimal.Neg(), e.Price.Decimal, nil
	default:
		return signed, price, fmt.Errorf("%w: %s is a fill with side %q",
			ErrMalformedEvent, e.VenueEventID, e.Side)
	}
}
