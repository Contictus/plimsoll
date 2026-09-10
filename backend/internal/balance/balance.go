// Package balance folds ledger events into what an account holds, asset by asset.
//
// It is the other half of the fold internal/position does. A position says what an
// exposure cost; a balance says what is actually there -- and for a spot account the
// second is the question the user is asking.
//
// Deltas is a pure function of an event and what the caller resolved for it (L4). It holds
// no database handle and no clock, which is what lets the same code answer "what do I hold
// now" and "what did I hold at T".
package balance

import (
	"errors"
	"fmt"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"
)

// The ways an event can fail to produce deltas. Each is an error rather than a skip: a hole
// in this fold surfaces later as a balance that is wrong by exactly one event, with nothing
// to say when it happened.
var (
	// ErrMalformedEvent means the event is missing something its type cannot be folded
	// without -- a deposit with no asset, a trade with no price.
	ErrMalformedEvent = errors.New("balance: event cannot be folded as written")

	// ErrUnresolved means the caller did not supply something the event needs: the
	// instrument's legs for a fill, or the fee asset for an event carrying a fee.
	ErrUnresolved = errors.New("balance: event names something that was not resolved")

	// ErrUnsupportedEventType means this engine has no rule for the type yet. Better a
	// halted fold than a quietly incomplete one -- and since 00009 the schema refuses to
	// store a type nothing can fold, so reaching this is defence in depth.
	ErrUnsupportedEventType = errors.New("balance: no fold rule for this event type")
)

// moneyScale matches NUMERIC(38,18), the scale internal/position rounds at. The one
// multiplication here rounds to it, so the engine and Postgres hold the same number (L1).
const moneyScale = 18

// Delta is one asset moving by one amount. Signed: positive is acquired, negative is spent.
type Delta struct {
	AssetID int64
	Amount  decimal.Decimal
}

// Legs are the two assets an instrument is made of. A fill moves both, in opposite
// directions, and naming them is the caller's job because resolving an instrument is a
// database read and this package does none.
type Legs struct {
	BaseAssetID  int64
	QuoteAssetID int64
}

// Resolved is what the caller looked up for one event.
type Resolved struct {
	// Legs is set when the event names an instrument. Nil otherwise, which is normal: a
	// deposit genuinely has no instrument.
	Legs *Legs

	// FeeAssetID is the asset the event's fee was paid in, resolved as of the event's own
	// event_time (L8). Nil when the event carries no fee -- or when it carries one whose
	// asset did not resolve, which is a different thing and is why Deltas refuses it
	// rather than silently dropping the fee.
	FeeAssetID *int64
}

// Deltas returns every asset movement one event causes, including its fee.
//
// The fee is a movement here, unlike in internal/position where it is kept apart from the
// entry price (K18, L9). Both are right: a fee never changes what a position cost, and it
// certainly changes what the account holds.
func Deltas(e ledger.Event, r Resolved) ([]Delta, error) {
	out, err := byType(e, r)
	if err != nil {
		return nil, err
	}

	if e.Fee.Valid && !e.Fee.Decimal.IsZero() {
		if r.FeeAssetID == nil {
			return nil, fmt.Errorf("%w: %s pays a fee in %q, which did not resolve",
				ErrUnresolved, e.VenueEventID, e.FeeAsset)
		}
		// Negated: the fee column holds what was paid, and a rebate arrives as a negative
		// fee, so one subtraction handles both and they never have to be added together.
		out = append(out, Delta{AssetID: *r.FeeAssetID, Amount: e.Fee.Decimal.Neg()})
	}
	return out, nil
}

func byType(e ledger.Event, r Resolved) ([]Delta, error) {
	switch e.EventType {
	case ledger.TypeTrade, ledger.TypeLiquidation:
		// A liquidation is a fill the exchange chose for you; the assets move the same way
		// and only the flag distinguishes them downstream (K6).
		return fill(e, r)

	case ledger.TypeDeposit:
		return single(e, e.Quantity, false)

	case ledger.TypeWithdrawal:
		// The quantity is what left, stated positively, so the balance effect is its
		// negation. The event type carries the direction; the number does not.
		return single(e, e.Quantity, true)

	case ledger.TypeTransfer:
		return transfer(e)

	case ledger.TypeFundingPayment:
		// A cash flow in the settle asset. V1 is USD-M only (PROJECT.md section 1), so the
		// settle asset is the quote asset and this is exact rather than an approximation.
		// Signed: a payment received is positive, one paid is negative.
		if r.Legs == nil {
			return nil, fmt.Errorf("%w: %s is a funding payment with no instrument legs",
				ErrUnresolved, e.VenueEventID)
		}
		if !e.Quantity.Valid {
			return nil, fmt.Errorf("%w: %s is a funding payment with no amount",
				ErrMalformedEvent, e.VenueEventID)
		}
		return []Delta{{AssetID: r.Legs.QuoteAssetID, Amount: e.Quantity.Decimal}}, nil

	case ledger.TypeFee, ledger.TypeCommissionRebate:
		// The whole of their effect is the fee, which Deltas applies for every type. A
		// standalone fee with nothing in its fee column moves nothing and is not an error:
		// it is an event that says nothing, and refusing it here would brick the fold over
		// a row the schema already accepted (00009).
		return nil, nil

	default:
		return nil, fmt.Errorf("%w: %s (%s)", ErrUnsupportedEventType, e.EventType, e.VenueEventID)
	}
}

// Wallet is one endpoint of a transfer. The vocabulary is closed and matches the schema's
// (00020); external is the only one that is not a wallet of this integration, and it is
// therefore the only one that makes a transfer move anything.
const (
	WalletExternal = "external"
)

// transfer folds a movement between two named wallets.
//
// The answer for the common case is nothing. asset_balances is keyed per integration, not
// per wallet, so moving USDT from spot to futures leaves the account holding exactly what it
// held. The event is history and lineage; it is not arithmetic. Reading it as a disposal is
// the error this milestone is named after -- it invents a realized loss, and then a phantom
// re-purchase when the money comes back.
//
// One side external is a real movement, and which side supplies the direction. Nothing
// writes that today; M8's cross-venue transfers do. The branch is here now because a rule
// that only ever ran on the internal case would have silently become "a transfer moves
// nothing", and the first withdrawal would have vanished.
func transfer(e ledger.Event) ([]Delta, error) {
	if e.TransferFrom == "" || e.TransferTo == "" {
		return nil, fmt.Errorf("%w: %s is a transfer that names %q -> %q",
			ErrMalformedEvent, e.VenueEventID, e.TransferFrom, e.TransferTo)
	}
	if e.TransferFrom == WalletExternal && e.TransferTo == WalletExternal {
		// Not this account's money moving, and not an internal transfer either: a
		// normalizer that lost track of which side of the wire it was on. Folding it to
		// nothing would hide that.
		return nil, fmt.Errorf("%w: %s transfers from outside to outside",
			ErrMalformedEvent, e.VenueEventID)
	}
	if !e.Quantity.Valid {
		return nil, fmt.Errorf("%w: %s transfers no amount",
			ErrMalformedEvent, e.VenueEventID)
	}
	if e.Quantity.Decimal.IsNegative() {
		// The retired convention showing up again. With the direction on the endpoints a
		// sign can only disagree with them, and the disagreement would be silent: a
		// withdrawal of -500 would read as money arriving.
		return nil, fmt.Errorf("%w: %s transfers a negative quantity; the endpoints carry the direction",
			ErrMalformedEvent, e.VenueEventID)
	}

	switch {
	case e.TransferTo == WalletExternal:
		return single(e, e.Quantity, true)
	case e.TransferFrom == WalletExternal:
		return single(e, e.Quantity, false)
	default:
		// Both wallets belong to this integration. The account holds what it held.
		return nil, nil
	}
}

// single moves the one asset the event names.
func single(e ledger.Event, quantity decimal.NullDecimal, negate bool) ([]Delta, error) {
	if e.AssetID == nil {
		return nil, fmt.Errorf("%w: %s moves an asset it does not name",
			ErrMalformedEvent, e.VenueEventID)
	}
	if !quantity.Valid {
		return nil, fmt.Errorf("%w: %s moves an asset by no amount",
			ErrMalformedEvent, e.VenueEventID)
	}
	amount := quantity.Decimal
	if negate {
		amount = amount.Neg()
	}
	return []Delta{{AssetID: *e.AssetID, Amount: amount}}, nil
}

// fill moves both legs of a trade in opposite directions: a buy acquires the base and
// spends the quote, a sell does the reverse.
//
// The quote leg is quantity times price, and that product is the one place this engine
// rounds. It rounds to the same scale the column holds, so a rebuild compares equal (L1, L3).
func fill(e ledger.Event, r Resolved) ([]Delta, error) {
	if r.Legs == nil {
		return nil, fmt.Errorf("%w: %s is a fill with no instrument legs",
			ErrUnresolved, e.VenueEventID)
	}
	if !e.Quantity.Valid || !e.Price.Valid {
		return nil, fmt.Errorf("%w: %s is a fill without both a quantity and a price",
			ErrMalformedEvent, e.VenueEventID)
	}

	base := e.Quantity.Decimal
	quote := base.Mul(e.Price.Decimal).Round(moneyScale)

	switch e.Side {
	case ledger.SideBuy:
		quote = quote.Neg()
	case ledger.SideSell:
		base = base.Neg()
	default:
		return nil, fmt.Errorf("%w: %s is a fill with no side",
			ErrMalformedEvent, e.VenueEventID)
	}
	return []Delta{
		{AssetID: r.Legs.BaseAssetID, Amount: base},
		{AssetID: r.Legs.QuoteAssetID, Amount: quote},
	}, nil
}
