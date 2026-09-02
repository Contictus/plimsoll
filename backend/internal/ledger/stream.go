package ledger

import (
	"context"
	"fmt"
	"math"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
)

// Stream returns up to limit events for one integration, in canonical order
// (event_time, venue_sequence, venue_event_id) -- L7 -- starting immediately after the
// given cursor. Pass the zero Cursor to start at the beginning, and the Cursor of the last
// event returned to continue.
//
// accountID is the primary defence and is passed even though RLS would filter anyway
// (L12); the integration is named separately because a projection advances per integration
// under the single-writer lease, never on the global seq (K20, L6).
func Stream(
	ctx context.Context,
	q *store.Queries,
	accountID uuid.UUID,
	integrationID uuid.UUID,
	after Cursor,
	limit int,
) ([]Event, error) {
	if limit <= 0 || limit > math.MaxInt32 {
		return nil, fmt.Errorf("ledger: stream limit must be between 1 and %d, got %d",
			math.MaxInt32, limit)
	}
	rows, err := q.StreamLedgerEvents(ctx, store.StreamLedgerEventsParams{
		AccountID:          accountID,
		IntegrationID:      integrationID,
		AfterEventTime:     after.EventTime,
		AfterVenueSequence: after.VenueSequence,
		AfterVenueEventID:  after.VenueEventID,
		MaxRows:            int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: stream integration %s: %w", integrationID, err)
	}

	events := make([]Event, 0, len(rows))
	for _, r := range rows {
		events = append(events, eventFromRow(r))
	}
	return events, nil
}

func eventFromRow(r store.LedgerEvent) Event {
	return Event{
		Seq:           r.Seq,
		AccountID:     r.AccountID,
		IntegrationID: r.IntegrationID,
		VenueEventID:  r.VenueEventID,
		VenueSequence: r.VenueSequence,
		Source:        r.Source,
		EventType:     EventType(r.EventType),
		InstrumentID:  r.InstrumentID,
		StrategyID:    r.StrategyID,
		Side:          Side(text(r.Side)),
		Quantity:      r.Quantity,
		Price:         r.Price,
		Fee:           r.Fee,
		FeeAsset:      text(r.FeeAsset),
		EventTime:     r.EventTime,
		IngestedAt:    r.IngestedAt,
		Raw:           r.Raw,
	}
}
