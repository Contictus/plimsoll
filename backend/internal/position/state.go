// Package position folds ledger events into a position. Apply is a pure function of state
// and event -- no database, no clock, no prices (L4) -- which is what lets a scenario shock
// and a historical reconstruction run the same code, and what keeps the tests Docker-free.
//
// Everything this package produces is a projection (L3): drop it, fold the ledger again,
// and the result must be identical.
package position

import (
	"sort"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"
)

// moneyScale matches NUMERIC(38,18). Every division rounds here rather than at
// shopspring's default of 16, so the value the engine computes and the value Postgres
// stores are the same number and a rebuild compares equal (L1, L3).
const moneyScale = 18

// FeeTotal is everything paid in one asset. Fees are kept per asset and unconverted: a fee
// in BNB against a USDT-denominated PnL needs a price, and this engine has none (K18, L9).
// Conversion is valuation's job, and a missing price there is reported as
// fee_price_missing rather than assumed (K23, L11).
type FeeTotal struct {
	Asset  string
	Amount decimal.Decimal
}

// State is one position: the exchange-style average-cost view of a single
// (integration, instrument) pair in one-way mode (K5). Quantity is signed -- positive
// long, negative short -- and AvgEntryPrice is always the unsigned price paid.
//
// The zero State is a flat position that has folded nothing, and it is the correct
// starting point for a rebuild.
type State struct {
	Quantity      decimal.Decimal
	AvgEntryPrice decimal.Decimal
	RealizedPnL   decimal.Decimal
	Fees          []FeeTotal

	// Cursor is the last event folded, in canonical order (L7). It is the whole triple
	// and not venue_sequence alone: two events can share a sequence, and a cursor that
	// compared only sequences would reject the second one and lose it permanently.
	Cursor ledger.Cursor
}

// after reports whether c comes strictly after other in canonical order
// (event_time, venue_sequence, venue_event_id) -- the same order the ledger reads in, so
// the fold and the query can never disagree about what "next" means.
func after(c, other ledger.Cursor) bool {
	if !c.EventTime.Equal(other.EventTime) {
		return c.EventTime.After(other.EventTime)
	}
	if c.VenueSequence != other.VenueSequence {
		return c.VenueSequence > other.VenueSequence
	}
	return c.VenueEventID > other.VenueEventID
}

// addFee returns a new slice with amount added to the asset's total, kept sorted by asset.
// It never writes into the slice it was given: State is passed by value, so mutating the
// backing array would reach back into the caller's state and make a rebuild depend on who
// folded first. The order is stable so that a projection serializes byte-identically.
func addFee(fees []FeeTotal, asset string, amount decimal.Decimal) []FeeTotal {
	next := make([]FeeTotal, len(fees), len(fees)+1)
	copy(next, fees)

	for i := range next {
		if next[i].Asset == asset {
			next[i].Amount = next[i].Amount.Add(amount)
			return next
		}
	}
	next = append(next, FeeTotal{Asset: asset, Amount: amount})
	sort.Slice(next, func(i, j int) bool { return next[i].Asset < next[j].Asset })
	return next
}
