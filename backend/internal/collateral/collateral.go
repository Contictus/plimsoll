// Package collateral answers one question: how far is this account from being liquidated.
//
// It is pure (L4). Every number it works from was captured from the exchange by someone
// else and handed in -- no database, no clock, no network -- which is what lets M7.5 shock
// the same functions with prices that never happened.
//
// What it deliberately does NOT do is compute a liquidation price. That depends on margin
// tier tables, cross versus isolated margin and wallet interactions that drift without
// notice, so it is read from the venue and only the DISTANCE is ours (K6). We inherit the
// exchange's staleness; a confidently wrong liquidation price is far more dangerous than a
// slightly late correct one.
package collateral

import (
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// ratioScale is the scale a ratio is rounded at. Wider than money's 18 would be pointless
// and narrower would lose basis points on a large notional, which is where the number is
// read most carefully.
const ratioScale = 18

var (
	// ErrOutsideBrackets means the notional is beyond the last tier the venue published.
	// The table ends where the exchange's own risk model ends: past it the position could
	// not have been opened, so answering anyway invents a number for a position that
	// cannot exist -- and invents it on the low side, which is the dangerous direction.
	ErrOutsideBrackets = errors.New("collateral: notional is outside the published brackets")

	// ErrSnapshotTorn means the two halves of a capture describe two different instants.
	ErrSnapshotTorn = errors.New("collateral: the snapshot's halves are too far apart")
)

// Bracket is one tier of the venue's maintenance-margin table (F15). Cum is the cumulative
// deduction that makes the tiers continuous rather than a step function jumping at every
// boundary.
type Bracket struct {
	Bracket          int
	NotionalFloor    decimal.Decimal
	NotionalCap      decimal.Decimal
	MaintMarginRatio decimal.Decimal
	Cum              decimal.Decimal
}

// PositionRisk is one position as the venue reported it. EntryPrice and Mark are here rather
// than taken from our own valuation on purpose: this struct describes the exchange's view,
// and comparing it against ours is what M7 does.
type PositionRisk struct {
	Symbol           string
	InstrumentID     int64
	Quantity         decimal.Decimal
	EntryPrice       decimal.Decimal
	MarkPrice        decimal.Decimal
	LiquidationPrice decimal.Decimal
	Notional         decimal.Decimal
	Leverage         decimal.Decimal
	MaintMargin      decimal.Decimal
}

// Snapshot is the exchange's answer about one instant. It is NOT a projection: it cannot be
// rebuilt from the ledger, because nothing in the ledger says what the exchange's margin
// engine believed at 12:00. That is why it is captured, stored, and given a freshness reason
// of its own rather than a rebuild-equality test (L3, L11).
type Snapshot struct {
	// AsOf is the EARLIER of the two calls that produced it. A snapshot is only as fresh
	// as its stalest half, and claiming otherwise would report a number as current on the
	// strength of the half that happened to be quick.
	AsOf time.Time

	MarginBalance     decimal.Decimal
	WalletBalance     decimal.Decimal
	UnrealizedPnL     decimal.Decimal
	MaintenanceMargin decimal.Decimal
	AvailableBalance  decimal.Decimal

	Positions []PositionRisk
	Brackets  map[string][]Bracket
}

// NewSnapshot pairs the two calls a capture is made of and refuses a torn one.
//
// Maintenance margin comes from the account endpoint and the liquidation price from
// positionRisk (F14) -- two calls, two instants. A margin buffer from 12:00:00 beside a
// liquidation price from 12:00:30 is one screen describing two different accounts, which is
// the failure L10 prevents for prices and nothing prevented for this until here.
func NewSnapshot(accountAt, positionsAt time.Time, tolerance time.Duration) (Snapshot, error) {
	gap := accountAt.Sub(positionsAt)
	if gap < 0 {
		gap = -gap
	}
	if gap > tolerance {
		return Snapshot{}, fmt.Errorf("%w: account at %s, positions at %s (%s apart)",
			ErrSnapshotTorn, accountAt.Format(time.RFC3339Nano),
			positionsAt.Format(time.RFC3339Nano), gap)
	}

	asOf := accountAt
	if positionsAt.Before(asOf) {
		asOf = positionsAt
	}
	return Snapshot{AsOf: asOf.UTC()}, nil
}

// Buffer is what is left before liquidation starts: margin balance minus the maintenance
// requirement.
//
// Signed, and never clamped. An account whose requirement already exceeds its balance is not
// "at zero" -- it is past the line, and how far past is the whole question. Clamping hides
// the one state the user most needs to see, at the one moment they are looking.
func Buffer(s Snapshot) decimal.Decimal {
	return s.MarginBalance.Sub(s.MaintenanceMargin)
}

// Distance is |mark - liquidation| / mark: how far the price can travel before the position
// is closed for you.
//
// Invalid rather than infinite when there is no liquidation price. A flat position has none
// and the venue reports zero for it; rendering that as "1.0" or "∞" teaches a reader to
// ignore the field on exactly the day it says something. Absent and far away are different
// claims and the type keeps them apart (L11).
func Distance(mark, liquidation decimal.Decimal) decimal.NullDecimal {
	if mark.IsZero() || liquidation.IsZero() {
		return decimal.NullDecimal{}
	}
	return decimal.NullDecimal{
		Decimal: mark.Sub(liquidation).Abs().Div(mark).Round(ratioScale),
		Valid:   true,
	}
}

// MaintenanceAt is the maintenance margin a notional requires, from the venue's tier table:
// `notional * rate - cum` (F15).
//
// This exists because M7.5 needs it. A scenario shock changes the notional, and a shock
// large enough to matter crosses into a higher rate -- so scaling today's maintenance margin
// by the price move, the obvious shortcut, silently assumes a constant rate and is wrong in
// the direction that understates the danger.
//
// The notional is taken as a size: a short's is negative in some of the venue's fields, and
// a negative maintenance requirement would read as free margin.
func MaintenanceAt(brackets []Bracket, notional decimal.Decimal) (decimal.Decimal, error) {
	if len(brackets) == 0 {
		// Not "no requirement" -- a capture that failed. Answering zero reports an account
		// with an infinite margin buffer at the exact moment we know least about it.
		return decimal.Decimal{}, fmt.Errorf(
			"%w: no bracket table was captured for this symbol", ErrOutsideBrackets)
	}

	size := notional.Abs()
	for _, b := range brackets {
		// Half-open [floor, cap): the boundary belongs to the bracket it opens, which is
		// how the venue's own cum values are constructed.
		if size.GreaterThanOrEqual(b.NotionalFloor) && size.LessThan(b.NotionalCap) {
			return size.Mul(b.MaintMarginRatio).Sub(b.Cum).Round(ratioScale), nil
		}
	}
	return decimal.Decimal{}, fmt.Errorf("%w: notional %s", ErrOutsideBrackets, size)
}
