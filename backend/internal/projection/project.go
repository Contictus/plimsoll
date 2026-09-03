// Package projection folds the ledger into the position tables and owns the cursor that
// says how far it has got.
//
// It exists as its own package so that internal/position stays what L4 says it is: a pure
// function of state and event, with no database handle anywhere in it. The I/O lives here,
// the arithmetic lives there, and the boundary is enforced by lint rather than by memory.
package projection

import (
	"context"
	"errors"
	"fmt"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// pageSize bounds how many events are held in memory at once. The states themselves are
// bounded by the instrument count, which is small; the event stream is not.
const pageSize = 500

// ErrUnknownIntegration means the integration named does not belong to this account. RLS
// would otherwise turn that into an empty result, and both Project and Rebuild would
// report success having done nothing -- Rebuild having also "dropped" a projection it
// could not see. Silence is the worst possible failure (L11).
var ErrUnknownIntegration = errors.New("projection: no such integration for this account")

// requireIntegration is called before any read or write, in the same transaction, so the
// answer cannot go stale between the check and the work.
func requireIntegration(ctx context.Context, q *store.Queries, accountID, integrationID uuid.UUID) error {
	ok, err := q.IntegrationExists(ctx, store.IntegrationExistsParams{
		AccountID: accountID, IntegrationID: integrationID,
	})
	if err != nil {
		return fmt.Errorf("projection: check integration %s: %w", integrationID, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownIntegration, integrationID)
	}
	return nil
}

// Result says what a projection run actually did. Rebuilt is the interesting field: it
// means events were found behind the cursor and the projection was rebuilt rather than
// folded forward, which M2's supervisor reports rather than hides.
type Result struct {
	EventsFolded int
	Rebuilt      bool
}

// Project folds every event appended since the last run into the position tables. It is
// idempotent: a second call finds its cursor where it left it and does no work.
//
// If it finds that events arrived *behind* the cursor -- which is exactly what happens
// when a backfill delivers last year's history while the live stream has already moved
// the cursor to today -- it rebuilds instead of folding forward. A keyset cursor cannot
// see backwards, and quietly leaving those events out of the position is the failure L11
// exists to prevent.
//
// The whole run is one transaction, so the rows and the cursor that describes them can
// never disagree. That is also its limit -- a first backfill of millions of events is one
// long transaction, and when M2 makes that real this is where the batching goes.
func Project(ctx context.Context, db tenancy.Beginner, accountID, integrationID uuid.UUID) (Result, error) {
	var res Result
	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		if err := requireIntegration(ctx, q, accountID, integrationID); err != nil {
			return err
		}
		behind, err := eventsArrivedBehindTheCursor(ctx, q, accountID, integrationID)
		if err != nil {
			return err
		}
		if behind {
			if err := dropProjection(ctx, q, accountID, integrationID); err != nil {
				return err
			}
			res.Rebuilt = true
		}
		res.EventsFolded, err = fold(ctx, q, accountID, integrationID)
		return err
	})
	return res, err
}

// eventsArrivedBehindTheCursor compares how many events sit at or below the cursor against
// how many the projector actually folded to get there.
//
// A count rather than a seq watermark, and the difference matters: seq values are assigned
// before commit, so a reader can pass one that is still in flight (K20, L6) and a watermark
// would miss precisely the case it was added for. A count is exact under any interleaving.
func eventsArrivedBehindTheCursor(
	ctx context.Context, q *store.Queries, accountID, integrationID uuid.UUID,
) (bool, error) {
	row, err := q.GetProjectionCursor(ctx, store.GetProjectionCursorParams{
		AccountID: accountID, IntegrationID: integrationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // nothing projected yet: there is nothing to be behind
	}
	if err != nil {
		return false, fmt.Errorf("projection: read cursor for %s: %w", integrationID, err)
	}

	actual, err := q.CountLedgerEventsThrough(ctx, store.CountLedgerEventsThroughParams{
		AccountID:     accountID,
		IntegrationID: integrationID,
		EventTime:     row.LastEventTime,
		VenueSequence: row.LastVenueSequence,
		VenueEventID:  row.LastVenueEventID,
	})
	if err != nil {
		return false, fmt.Errorf("projection: count events for %s: %w", integrationID, err)
	}
	return actual != row.ProjectedCount, nil
}

func dropProjection(ctx context.Context, q *store.Queries, accountID, integrationID uuid.UUID) error {
	if err := q.DropProjection(ctx, store.DropProjectionParams{
		AccountID: accountID, IntegrationID: integrationID,
	}); err != nil {
		return fmt.Errorf("projection: drop positions for %s: %w", integrationID, err)
	}
	if err := q.DropProjectionCursor(ctx, store.DropProjectionCursorParams{
		AccountID: accountID, IntegrationID: integrationID,
	}); err != nil {
		return fmt.Errorf("projection: drop cursor for %s: %w", integrationID, err)
	}
	return nil
}

// Rebuild drops the projection and folds it again from the first event. Every projection
// must survive this and produce identical rows (L3); a projection that cannot be rebuilt
// has quietly become a second source of truth.
//
// The drop and the fold share one transaction, so a rebuild that fails halfway leaves the
// old projection in place rather than nothing at all. position_strategies is deliberately
// untouched: the strategy tag is user input, not a fold output (D2).
func Rebuild(ctx context.Context, db tenancy.Beginner, accountID, integrationID uuid.UUID) error {
	return tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		if err := requireIntegration(ctx, q, accountID, integrationID); err != nil {
			return err
		}
		if err := dropProjection(ctx, q, accountID, integrationID); err != nil {
			return err
		}
		_, err := fold(ctx, q, accountID, integrationID)
		return err
	})
}

// fold reads forward from the cursor and returns how many events it consumed. The count
// travels with the cursor so the next run can tell whether anything arrived behind it.
func fold(ctx context.Context, q *store.Queries, accountID, integrationID uuid.UUID) (int, error) {
	cursor, projected, err := loadCursor(ctx, q, accountID, integrationID)
	if err != nil {
		return 0, err
	}
	states, err := loadStates(ctx, q, accountID, integrationID)
	if err != nil {
		return 0, err
	}

	touched := map[int64]bool{}
	folded := 0

	for {
		events, err := ledger.Stream(ctx, q, accountID, integrationID, cursor, pageSize)
		if err != nil {
			return folded, err
		}
		if len(events) == 0 {
			break
		}
		for _, e := range events {
			// The cursor advances for every event, including those that touch no
			// position. Skipping them would make the projector reread them forever.
			cursor = e.Cursor()
			folded++

			if e.InstrumentID == nil {
				// A balance event genuinely has no instrument and changes no position, so
				// it advances the cursor and nothing else. Anything that carries value is
				// refused at write time by value_events_name_their_instrument (00009), so
				// this branch cannot reach one -- and if the schema ever relaxes without
				// this being revisited, the error says so instead of the money quietly
				// leaving the numbers.
				switch e.EventType {
				case ledger.TypeDeposit, ledger.TypeWithdrawal, ledger.TypeTransfer:
					continue
				default:
					return folded, fmt.Errorf(
						"projection: %s event %s carries value but names no instrument",
						e.EventType, e.VenueEventID)
				}
			}
			next, err := position.Apply(states[*e.InstrumentID], e)
			if err != nil {
				return folded, fmt.Errorf("projection: folding %s into instrument %d: %w",
					e.VenueEventID, *e.InstrumentID, err)
			}
			states[*e.InstrumentID] = next
			touched[*e.InstrumentID] = true
		}
		if len(events) < pageSize {
			break
		}
	}

	if folded == 0 {
		return 0, nil
	}
	for instrumentID := range touched {
		if err := writePosition(ctx, q, accountID, integrationID, instrumentID, states[instrumentID]); err != nil {
			return folded, err
		}
	}
	if err := q.UpsertProjectionCursor(ctx, store.UpsertProjectionCursorParams{
		AccountID:         accountID,
		IntegrationID:     integrationID,
		LastEventTime:     cursor.EventTime,
		LastVenueSequence: cursor.VenueSequence,
		LastVenueEventID:  cursor.VenueEventID,
		ProjectedCount:    projected + int64(folded),
	}); err != nil {
		return folded, fmt.Errorf("projection: advance cursor for %s: %w", integrationID, err)
	}
	return folded, nil
}
