package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
)

// ErrNoSuchPosition means the id names nothing this account holds. It is deliberately the
// same answer for a position that does not exist and one that belongs to somebody else:
// telling a caller which is which tells them about an account they cannot see.
var ErrNoSuchPosition = errors.New("portfolio: no such position")

// lineagePage bounds how many events are read from the database at once. The replay itself
// walks the whole history -- it has to, because an intermediate state is only correct if
// every event before it was folded -- but it never holds more than this many rows.
const lineagePage = 500

// defaultLineageSteps is how many steps are returned when the caller asks for no particular
// number. Enough to explain a position, small enough to read.
const defaultLineageSteps = 200

// maxLineageSteps caps what a caller may ask for. The replay's cost is the account's whole
// history either way; this bounds only the response.
const maxLineageSteps = 1000

// Step is one event and the position it produced. Both halves matter: the event alone says
// what the exchange reported, and the state alone says where the number came out. Together
// they are the answer to "why is this number what it is", which is the product thesis in
// one struct (ARCHITECTURE.md section 10, rule 6).
type Step struct {
	Event ledger.Event

	// Quantity, AvgEntryPrice and RealizedPnL are the position *after* this event was
	// folded, produced by the same position.Apply the projector runs. Not a re-derivation:
	// the same code, so a lineage that disagrees with a position is a real finding rather
	// than two implementations drifting.
	Quantity      string
	AvgEntryPrice string
	RealizedPnL   string
}

// Lineage is one position opened down to the events that produced it.
type Lineage struct {
	AsOf     time.Time
	Position Holding

	// Steps are the last TotalEvents-minus-offset events, most recent last. The tail rather
	// than the head because the recent end is what a reader is asking about; the whole
	// history is still folded to get there.
	Steps       []Step
	TotalEvents int

	Freshness freshness.Report
}

// LoadLineage opens one position down to its events.
//
// It replays every event from the first, through the same engine the projector uses, and
// then checks the result against the stored projection. That check is the point of the
// endpoint: if the events do not reproduce the number, the response says so rather than
// serving a number it has just disproved (L11).
//
// The cost is the position's whole history per call. That is honest for M3 and is what
// position_snapshots exists to fix later (ARCHITECTURE.md section 3) -- a snapshot is a
// cache, so adding one must change the timing and nothing else.
func LoadLineage(
	ctx context.Context,
	db tenancy.Beginner,
	accountID uuid.UUID,
	id string,
	steps int,
	now time.Time,
	leaseTTL, priceTTL time.Duration,
) (Lineage, error) {
	integrationID, instrumentID, err := ParsePositionID(id)
	if err != nil {
		return Lineage{}, err
	}
	if steps <= 0 {
		steps = defaultLineageSteps
	}
	if steps > maxLineageSteps {
		steps = maxLineageSteps
	}

	var out Lineage
	err = tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		in, err := read(ctx, q, accountID, now, leaseTTL, priceTTL)
		if err != nil {
			return err
		}
		holding, found := Build(in).Find(id)
		if !found {
			return fmt.Errorf("%w: %s", ErrNoSuchPosition, id)
		}
		out, err = replay(ctx, q, accountID, integrationID, instrumentID, steps)
		if err != nil {
			return err
		}
		out.AsOf = now
		out.Position = holding
		out.Freshness = freshness.New(append(in.Reasons, disagreement(holding, out)...)...)
		return nil
	})
	return out, err
}

// replay folds the position's whole history and keeps the last `steps` of it.
//
// A ring rather than a slice of everything: the intermediate states are only correct if
// every earlier event was folded, so the walk cannot be shortened -- but the response can,
// and holding a decade of fills in memory to serve two hundred of them would make the
// endpoint's memory a property of the account rather than of the request.
func replay(
	ctx context.Context,
	q *store.Queries,
	accountID, integrationID uuid.UUID,
	instrumentID int64,
	steps int,
) (Lineage, error) {
	var (
		state  position.State
		cursor ledger.Cursor
		ring   = make([]Step, 0, steps)
		total  int
	)

	for {
		rows, err := q.ListPositionEventsAfter(ctx, store.ListPositionEventsAfterParams{
			AccountID:          accountID,
			IntegrationID:      integrationID,
			InstrumentID:       &instrumentID,
			AfterEventTime:     cursor.EventTime,
			AfterVenueSequence: cursor.VenueSequence,
			AfterVenueEventID:  cursor.VenueEventID,
			MaxRows:            lineagePage,
		})
		if err != nil {
			return Lineage{}, fmt.Errorf("portfolio: read lineage for %s: %w",
				PositionID(integrationID, instrumentID), err)
		}
		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			event := eventOf(row, accountID, integrationID)
			next, err := position.Apply(state, event)
			if err != nil {
				return Lineage{}, fmt.Errorf("portfolio: replaying %s into %s: %w",
					event.VenueEventID, PositionID(integrationID, instrumentID), err)
			}
			state = next
			cursor = event.Cursor()
			total++

			if len(ring) == steps {
				ring = ring[1:]
			}
			ring = append(ring, Step{
				Event:         event,
				Quantity:      state.Quantity.String(),
				AvgEntryPrice: state.AvgEntryPrice.String(),
				RealizedPnL:   state.RealizedPnL.String(),
			})
		}
		if len(rows) < lineagePage {
			break
		}
	}
	return Lineage{Steps: ring, TotalEvents: total}, nil
}

// eventOf rebuilds the canonical event from its row. The nullable columns are pointers in
// the generated types and values in the engine's, and this is the one place that gap is
// crossed -- so a NULL side becomes the empty Side rather than a panic somewhere later.
func eventOf(row store.ListPositionEventsAfterRow, accountID, integrationID uuid.UUID) ledger.Event {
	event := ledger.Event{
		Seq:           row.Seq,
		AccountID:     accountID,
		IntegrationID: integrationID,
		VenueEventID:  row.VenueEventID,
		VenueSequence: row.VenueSequence,
		Source:        row.Source,
		EventType:     ledger.EventType(row.EventType),
		InstrumentID:  row.InstrumentID,
		AssetID:       row.AssetID,
		StrategyID:    row.StrategyID,
		Quantity:      row.Quantity,
		Price:         row.Price,
		Fee:           row.Fee,
		EventTime:     row.EventTime,
		IngestedAt:    row.IngestedAt,
		Raw:           json.RawMessage(row.Raw),
	}
	if row.Side != nil {
		event.Side = ledger.Side(*row.Side)
	}
	if row.FeeAsset != nil {
		event.FeeAsset = *row.FeeAsset
	}
	event.FeeAssetID = row.FeeAssetID
	return event
}

// disagreement compares the replay against the stored projection and reports it if they
// differ. This is the check the endpoint exists to make possible, and the day it fires is
// the day the design earns itself.
//
// It only compares when the replay ended on the same event the projection did. A projection
// that is behind is expected -- the fold runs on a ticker (K38) -- and is already reported
// as projection_lagging; calling that a disagreement would make the serious signal fire
// constantly and stop meaning anything.
func disagreement(stored Holding, replayed Lineage) []freshness.Reason {
	if len(replayed.Steps) == 0 {
		return nil
	}
	last := replayed.Steps[len(replayed.Steps)-1]
	if !last.Event.EventTime.Equal(stored.LastEventTime) {
		return nil
	}

	same := last.Quantity == stored.Quantity.String() &&
		last.AvgEntryPrice == stored.AvgEntryPrice.String() &&
		last.RealizedPnL == stored.RealizedPnL.String()
	if same {
		return nil
	}
	return []freshness.Reason{{
		Code:     freshness.ReasonLineageMismatch,
		Severity: freshness.SeverityError,
		Detail: fmt.Sprintf(
			"replaying this position's events produces quantity %s, entry %s, realized %s;"+
				" the stored projection holds %s, %s, %s",
			last.Quantity, last.AvgEntryPrice, last.RealizedPnL,
			stored.Quantity, stored.AvgEntryPrice, stored.RealizedPnL),
		Since: replayed.AsOf,
	}}
}
