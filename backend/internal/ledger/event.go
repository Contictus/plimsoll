// Package ledger owns the append-only event log: the canonical Event type, the write that
// deduplicates on venue identity, and the ordered read the projections fold over.
//
// Unlike the calculation engines it does hold a database handle, but only a
// transaction-scoped *store.Queries handed to it by tenancy.InTx -- never a pool, never a
// clock, never a logger. Everything derived from these events is a projection (L3); this
// package is the only thing that is not.
package ledger

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// EventType is what happened. The set is closed: a normalizer that meets something it
// cannot classify must raise it rather than invent a category, because an event stored
// under the wrong type folds into the wrong number.
type EventType string

// The canonical event types (PROJECT.md §2).
const (
	TypeTrade              EventType = "TRADE"
	TypeTransfer           EventType = "TRANSFER"
	TypeDeposit            EventType = "DEPOSIT"
	TypeWithdrawal         EventType = "WITHDRAWAL"
	TypeFundingPayment     EventType = "FUNDING_PAYMENT"
	TypeFee                EventType = "FEE"
	TypeCommissionRebate   EventType = "COMMISSION_REBATE"
	TypeLiquidation        EventType = "LIQUIDATION"
	TypePositionAdjustment EventType = "POSITION_ADJUSTMENT"
)

// Side is the direction of a fill. The empty value means the event has no direction, which
// is normal for a deposit or a funding payment.
type Side string

// The two sides a fill can take. V1 is one-way mode only (K5).
const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// Event is one canonical fact about an account, as reported by a venue. Money fields are
// decimal.NullDecimal rather than decimal.Decimal (L1): a missing price and a price of
// zero are different claims, and collapsing them is how a deposit acquires a cost basis.
//
// Seq and IngestedAt are assigned by the database and are read-only -- they are lineage,
// never inputs to a calculation.
type Event struct {
	Seq           int64
	AccountID     uuid.UUID
	IntegrationID uuid.UUID

	// VenueEventID is the event's identity, built by the normalizer from exchange fields
	// alone: <market>:<kind>:<symbol>:<venue id>. Source is excluded from it on purpose
	// (K19, L5).
	VenueEventID  string
	VenueSequence int64
	Source        string

	EventType    EventType
	InstrumentID *int64

	// AssetID names what moved, for the events that move one asset rather than trade a
	// pair. A trade leaves it nil: its instrument already names both legs, and a second
	// copy here would be a second place for them to disagree (00012).
	AssetID    *int64
	StrategyID *uuid.UUID
	Side       Side

	Quantity decimal.NullDecimal
	Price    decimal.NullDecimal

	// Fee belongs to this event and is never folded into the average entry price (K18, L9).
	Fee      decimal.NullDecimal
	FeeAsset string

	EventTime  time.Time
	IngestedAt time.Time

	// Raw is the exchange payload, kept forever (L15). An event without one cannot be
	// stored; the column is NOT NULL and this package does not paper over that.
	Raw json.RawMessage
}

// Cursor is a position in the canonical order (event_time, venue_sequence, venue_event_id)
// -- L7. It is what a projection advances on, because the global seq cannot be used for
// that: identity values are assigned before commit, so a reader can observe a later seq
// while an earlier one is still in flight and skip it permanently (K20, L6).
//
// The zero Cursor sits before every event.
type Cursor struct {
	EventTime     time.Time
	VenueSequence int64
	VenueEventID  string
}

// Cursor returns the position of this event, for use as the `after` argument of the next
// Stream call.
func (e Event) Cursor() Cursor {
	return Cursor{
		EventTime:     e.EventTime,
		VenueSequence: e.VenueSequence,
		VenueEventID:  e.VenueEventID,
	}
}
