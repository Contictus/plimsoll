package portfolio

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ErrMalformedCursor means the cursor did not come from this API. Returned rather than
// silently restarting from the beginning: a client that pages with a corrupted cursor and
// is quietly served page one would keep looping and never know.
var ErrMalformedCursor = errors.New("portfolio: malformed cursor")

const (
	defaultTransactionLimit = 100
	maxTransactionLimit     = 500
)

// cursor is a position in the account's ledger, in canonical order with integration_id as
// the last tiebreak. Two integrations can mint the same venue_event_id -- the id is built
// from exchange fields alone (K19, L5), and two connections to one exchange see the same
// exchange -- so a cursor without it would stop at the first of them forever.
//
// Opaque to the client on purpose: it is base64 of a JSON object, and making it look
// unstructured is what lets its shape change without breaking anyone. Never `seq`: identity
// values are assigned before commit, so a seq cursor skips rows that were in flight
// (K20, K41).
type cursor struct {
	EventTime     time.Time `json:"t"`
	VenueSequence int64     `json:"s"`
	VenueEventID  string    `json:"v"`
	IntegrationID uuid.UUID `json:"i"`
}

func (c cursor) encode() string {
	// Cannot fail: every field marshals.
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil // the zero cursor sits before every event
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: not base64", ErrMalformedCursor)
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return cursor{}, fmt.Errorf("%w: not a cursor", ErrMalformedCursor)
	}
	return c, nil
}

// Transaction is one ledger event as a reader sees it. Money arrives as a string for the
// same reason it does everywhere else (L1), and the optional fields are strings rather than
// pointers-to-decimal because "" and "0" are different claims and both are renderable.
type Transaction struct {
	// Seq is lineage and never a cursor (K20). It is what a support conversation quotes.
	Seq int64

	IntegrationID uuid.UUID
	VenueEventID  string
	VenueSequence int64

	// Source is who saw it first -- REST backfill or the live stream. Metadata, never
	// identity: both paths report the same trade under the same venue_event_id (K19, L5).
	Source string

	EventType string
	Side      string

	// Instrument and Asset are whichever the event names. A trade names an instrument; a
	// deposit names an asset. Never both (00012).
	Instrument string
	Asset      string

	Quantity string
	Price    string
	Fee      string
	FeeAsset string

	EventTime  time.Time
	IngestedAt time.Time
}

// Transactions is one page of the account's ledger.
type Transactions struct {
	AsOf   time.Time
	Events []Transaction

	// NextCursor is empty when the page reached the end of what exists now. "Now" is the
	// honest qualifier: a backfill inserts events *behind* a cursor a reader has already
	// passed, and no ordering avoids that while history is still loading. What the response
	// does instead is carry backfill_incomplete, so a client knows its page is a view of an
	// incomplete set rather than a final one (K41).
	NextCursor string

	Freshness freshness.Report
}

func text(d decimal.NullDecimal) string {
	if !d.Valid {
		return ""
	}
	return d.Decimal.String()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// LoadTransactions returns one page of the account's ledger, newest last, from an opaque
// cursor. It reads the freshness in the same transaction as the events, so a page and the
// warning that qualifies it describe the same instant (L10).
func LoadTransactions(
	ctx context.Context,
	db tenancy.Beginner,
	accountID uuid.UUID,
	after string,
	limit int,
	now time.Time,
	leaseTTL time.Duration,
) (Transactions, error) {
	from, err := decodeCursor(after)
	if err != nil {
		return Transactions{}, err
	}
	switch {
	case limit <= 0:
		limit = defaultTransactionLimit
	case limit > maxTransactionLimit:
		limit = maxTransactionLimit
	}

	var out Transactions
	err = tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		rows, err := q.StreamAccountEvents(ctx, store.StreamAccountEventsParams{
			AccountID:          accountID,
			AfterEventTime:     from.EventTime,
			AfterVenueSequence: from.VenueSequence,
			AfterVenueEventID:  from.VenueEventID,
			AfterIntegrationID: from.IntegrationID,
			MaxRows:            int32(limit), //nolint:gosec // bounded by maxTransactionLimit above
		})
		if err != nil {
			return fmt.Errorf("portfolio: read transactions for %s: %w", accountID, err)
		}

		out.Events = make([]Transaction, 0, len(rows))
		for _, r := range rows {
			out.Events = append(out.Events, Transaction{
				Seq:           r.Seq,
				IntegrationID: r.IntegrationID,
				VenueEventID:  r.VenueEventID,
				VenueSequence: r.VenueSequence,
				Source:        r.Source,
				EventType:     r.EventType,
				Side:          deref(r.Side),
				Instrument:    deref(r.InstrumentSymbol),
				Asset:         deref(r.AssetSymbol),
				Quantity:      text(r.Quantity),
				Price:         text(r.Price),
				Fee:           text(r.Fee),
				FeeAsset:      deref(r.FeeAsset),
				EventTime:     r.EventTime,
				IngestedAt:    r.IngestedAt,
			})
		}
		// A full page means there may be more; a short one means there is nothing further
		// right now. Offering a cursor after a short page would invite a client to poll it,
		// which is a different feature and one this endpoint does not claim to be.
		if len(rows) == limit {
			last := rows[len(rows)-1]
			out.NextCursor = cursor{
				EventTime:     last.EventTime,
				VenueSequence: last.VenueSequence,
				VenueEventID:  last.VenueEventID,
				IntegrationID: last.IntegrationID,
			}.encode()
		}

		statuses, err := ingest.StatusIn(ctx, q, accountID)
		if err != nil {
			return err
		}
		out.AsOf = now
		out.Freshness = freshness.New(ReasonsFor(statuses, nil, now, leaseTTL)...)
		return nil
	})
	return out, err
}
