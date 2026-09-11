// Package scenario answers the one question a leveraged trader asks that the rest of the
// system cannot: what happens to me if the price moves?
//
// It is a pure function of (wallet balance, positions, holdings, shocks) -- no database, no
// clock, no network (L4). That is why it is cheap: every engine underneath it was already
// pure, and this one only has to do the arithmetic honestly.
//
// It models. It never places an order and it writes nothing (L13).
package scenario

import (
	"errors"
	"fmt"
	"sort"

	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/shopspring/decimal"
)

// ErrImpossibleMove means a shock would take a price to zero or below.
//
// Refused rather than clamped: clamping answers a question the user did not ask and then
// presents the answer as though it were the one they did.
var ErrImpossibleMove = errors.New("scenario: a move at or below -1 would take the price to zero or below")

// Position is one perpetual, as the venue last reported it.
type Position struct {
	Symbol    string
	BaseAsset string

	// Quantity is signed: a short's is negative, and that sign is what makes a hedge cancel.
	Quantity   decimal.Decimal
	EntryPrice decimal.Decimal
	MarkPrice  decimal.Decimal

	// Brackets is the venue's maintenance tier table for this symbol. Empty means it was
	// never captured, which makes the whole buffer unavailable rather than smaller (K56).
	Brackets []collateral.Bracket
}

// Holding is one spot asset. It is here, beside the positions, because a shock hits both legs
// of a hedge or it reports a loss the user does not have (K56).
type Holding struct {
	Asset    string
	Quantity decimal.Decimal

	// Price is in the numeraire and INVALID when the run could not price the asset. Invalid is
	// not zero: an unpriced holding is excluded and named, never valued at nothing.
	Price decimal.NullDecimal
}

// Input is one projection's whole world.
type Input struct {
	// WalletBalance is realized cash in the futures wallet. It does not move under a price
	// shock -- that is what makes it the anchor the rest is measured from.
	WalletBalance decimal.Decimal

	Positions []Position
	Holdings  []Holding

	// Shocks is a signed fraction per asset: -0.2 is minus twenty percent. An asset absent
	// from the map holds still, and that is the entire content of K56.
	Shocks map[string]decimal.Decimal
}

// Outcome is the account at one price, before or after.
type Outcome struct {
	Equity        decimal.Decimal
	UnrealizedPnL decimal.Decimal
	MarginBalance decimal.Decimal

	// MaintenanceMargin and Buffer are invalid together: a requirement summed from only the
	// positions we could price understates it, which overstates the buffer (K56).
	MaintenanceMargin decimal.NullDecimal
	Buffer            decimal.NullDecimal

	// Liquidated is false when the buffer is unavailable. A buffer nobody could compute must
	// not be reported as a liquidation, and must not be reported as safety either.
	Liquidated bool
}

// Report is the before and the after, with everything neither could account for.
type Report struct {
	Base    Outcome
	Shocked Outcome

	// Unavailable names every position whose requirement could not be computed and every
	// holding that could not be priced, so a reader knows what the numbers leave out (L11).
	Unavailable []string
}

// Project measures the account as it stands and as it would stand under the shocks.
func Project(in Input) (Report, error) {
	for asset, move := range in.Shocks {
		if move.LessThanOrEqual(decimal.NewFromInt(-1)) {
			return Report{}, fmt.Errorf("%w: %s %s", ErrImpossibleMove, asset, move)
		}
	}

	unavailable := map[string]struct{}{}
	base := measure(in, false, unavailable)
	shocked := measure(in, true, unavailable)

	return Report{Base: base, Shocked: shocked, Unavailable: sortedKeys(unavailable)}, nil
}

// measure values the account at one set of prices. `shocked` selects which.
//
// The two passes are the same code deliberately: a base case computed by a different path from
// the shocked one would let the difference between them come from the code rather than from
// the shock, which is the one thing the whole report claims it does not.
func measure(in Input, shocked bool, unavailable map[string]struct{}) Outcome {
	var out Outcome
	maintenance := decimal.Zero
	requirementKnown := true

	for _, p := range in.Positions {
		mark := priceUnder(p.MarkPrice, p.BaseAsset, in.Shocks, shocked)

		// Unrealized PnL is (mark - entry) * quantity, and the sign of quantity carries the
		// direction: a short's PnL rises as the mark falls, without a branch to get wrong.
		out.UnrealizedPnL = out.UnrealizedPnL.Add(mark.Sub(p.EntryPrice).Mul(p.Quantity))

		required, err := collateral.MaintenanceAt(p.Brackets, p.Quantity.Mul(mark))
		if err != nil {
			// Named once, for both passes -- the table is missing at every price.
			unavailable[p.Symbol] = struct{}{}
			requirementKnown = false
			continue
		}
		maintenance = maintenance.Add(required)
	}

	spot := decimal.Zero
	for _, h := range in.Holdings {
		if !h.Price.Valid {
			unavailable[h.Asset] = struct{}{}
			continue
		}
		spot = spot.Add(h.Quantity.Mul(priceUnder(h.Price.Decimal, h.Asset, in.Shocks, shocked)))
	}

	out.MarginBalance = in.WalletBalance.Add(out.UnrealizedPnL)
	out.Equity = out.MarginBalance.Add(spot)

	if requirementKnown {
		out.MaintenanceMargin = decimal.NewNullDecimal(maintenance)
		buffer := out.MarginBalance.Sub(maintenance)
		out.Buffer = decimal.NewNullDecimal(buffer)
		// Signed and never clamped: an account past the line is not "at zero", and how far
		// past is the whole question (K50).
		out.Liquidated = buffer.IsNegative()
	}
	return out
}

// priceUnder applies the asset's own shock, or none. An asset absent from the map holds still:
// inventing a correlation the user did not ask for produces a number that looks like analysis
// and is a guess (K56).
func priceUnder(
	price decimal.Decimal, asset string, shocks map[string]decimal.Decimal, shocked bool,
) decimal.Decimal {
	if !shocked {
		return price
	}
	move, ok := shocks[asset]
	if !ok {
		return price
	}
	return price.Mul(decimal.NewFromInt(1).Add(move))
}

// sortedKeys keeps the report stable: ranging a map directly would reorder Unavailable between
// two projections of the same book.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
