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

// Project folds every event appended since the last run into the position tables. It is
// idempotent: a second call finds its cursor where it left it and does no work.
//
// The whole run is one transaction, so the rows and the cursor that describes them can
// never disagree. That is also its limit -- a first backfill of millions of events is one
// long transaction, and when M2 makes that real this is where the batching goes.
func Project(ctx context.Context, db tenancy.Beginner, accountID, integrationID uuid.UUID) error {
	return tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		if err := requireIntegration(ctx, q, accountID, integrationID); err != nil {
			return err
		}
		return fold(ctx, q, accountID, integrationID)
	})
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
		return fold(ctx, q, accountID, integrationID)
	})
}

func fold(ctx context.Context, q *store.Queries, accountID, integrationID uuid.UUID) error {
	cursor, err := loadCursor(ctx, q, accountID, integrationID)
	if err != nil {
		return err
	}
	states, err := loadStates(ctx, q, accountID, integrationID)
	if err != nil {
		return err
	}

	touched := map[int64]bool{}
	advanced := false

	for {
		events, err := ledger.Stream(ctx, q, accountID, integrationID, cursor, pageSize)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			break
		}
		for _, e := range events {
			// The cursor advances for every event, including those that touch no
			// position. Skipping them would make the projector reread them forever.
			cursor = e.Cursor()
			advanced = true

			if e.InstrumentID == nil {
				continue
			}
			next, err := position.Apply(states[*e.InstrumentID], e)
			if err != nil {
				return fmt.Errorf("projection: folding %s into instrument %d: %w",
					e.VenueEventID, *e.InstrumentID, err)
			}
			states[*e.InstrumentID] = next
			touched[*e.InstrumentID] = true
		}
		if len(events) < pageSize {
			break
		}
	}

	if !advanced {
		return nil
	}
	for instrumentID := range touched {
		if err := writePosition(ctx, q, accountID, integrationID, instrumentID, states[instrumentID]); err != nil {
			return err
		}
	}
	if err := q.UpsertProjectionCursor(ctx, store.UpsertProjectionCursorParams{
		AccountID:         accountID,
		IntegrationID:     integrationID,
		LastEventTime:     cursor.EventTime,
		LastVenueSequence: cursor.VenueSequence,
		LastVenueEventID:  cursor.VenueEventID,
	}); err != nil {
		return fmt.Errorf("projection: advance cursor for %s: %w", integrationID, err)
	}
	return nil
}
