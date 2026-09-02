package ledger

import (
	"context"
	"fmt"

	"github.com/Contictus/plimsoll/backend/internal/store"
)

// Append writes events that are not already present and reports how many rows it actually
// inserted. The caller is expected to compare that against len(events): "we saw 40 events
// and stored 12" is the signal that makes a duplicate backfill visible instead of silent.
//
// Deduplication is UNIQUE (integration_id, venue_event_id) plus ON CONFLICT DO NOTHING,
// which is a correctness mechanism rather than a performance trick only because
// venue_event_id is built from exchange fields alone (K19, L5). The same trade arriving
// from REST and from the WebSocket stream is therefore stored once, and source records
// which path saw it first.
//
// q must come from tenancy.InTx: the account is bound on the transaction, so a row
// attributed to another account is refused by the RLS WITH CHECK before the composite
// foreign key is ever consulted.
func Append(ctx context.Context, q *store.Queries, events []Event) (int, error) {
	inserted := 0
	for _, e := range events {
		rows, err := q.InsertLedgerEvent(ctx, e.insertParams())
		if err != nil {
			// The venue identity is what a reader needs to find the offending payload;
			// nothing here echoes a credential (L13).
			return inserted, fmt.Errorf("ledger: append %s (integration %s): %w",
				e.VenueEventID, e.IntegrationID, err)
		}
		inserted += int(rows)
	}
	return inserted, nil
}

// insertParams maps the canonical event onto the generated parameters. The empty string is
// the absent value for the two optional text columns, so a caller never has to build a
// pointer to say "this event has no side".
func (e Event) insertParams() store.InsertLedgerEventParams {
	return store.InsertLedgerEventParams{
		AccountID:     e.AccountID,
		IntegrationID: e.IntegrationID,
		VenueEventID:  e.VenueEventID,
		VenueSequence: e.VenueSequence,
		Source:        e.Source,
		EventType:     string(e.EventType),
		InstrumentID:  e.InstrumentID,
		StrategyID:    e.StrategyID,
		Side:          optionalText(string(e.Side)),
		Quantity:      e.Quantity,
		Price:         e.Price,
		Fee:           e.Fee,
		FeeAsset:      optionalText(e.FeeAsset),
		EventTime:     e.EventTime,
		Raw:           e.Raw,
	}
}

func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func text(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
