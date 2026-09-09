package projection

import (
	"context"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// This file answers "what did this account hold at T" without writing anything.
//
// It lives beside the projector rather than in the read model because it must fold through
// the *same* engines the projector does -- position.Apply and balance.Deltas, by way of
// applyBalance. A second implementation of the fold would make a disagreement between the
// live portfolio and the one at T a bug in the reader rather than a finding, which is
// exactly backwards: the whole product claim is that the numbers reproduce (L3).

// PositionKey and BalanceKey are the projection's own keys, not surrogates: a rebuild
// regenerates ids, and an answer that changed after a rebuild would not be an answer (K42).
type PositionKey struct {
	IntegrationID uuid.UUID
	InstrumentID  int64
}

// BalanceKey is the same idea for the asset fold.
type BalanceKey struct {
	IntegrationID uuid.UUID
	AssetID       int64
}

// At is the account's folded state at one instant, held in memory and stored nowhere.
//
// Nothing here is persisted, and it does not need to be: the ledger is durable and the fold
// is pure, so the same T yields the same state forever. Storing it would grow a table with
// one row per curious click and add nothing an audit could not already reproduce (K48).
type At struct {
	Positions map[PositionKey]position.State
	Balances  map[BalanceKey]BalanceAt

	// Events is how many events were folded to get here. It is the honest cost of this
	// answer, and the number that says when position_snapshots has become necessary rather
	// than merely anticipated (ARCHITECTURE.md section 3).
	Events int
}

// BalanceAt is one asset's quantity and the event that last moved it.
type BalanceAt struct {
	Quantity decimal.Decimal
	Cursor   ledger.Cursor
}

// FoldAt replays every event with an event_time at or before `at` and returns the state it
// produces. Nothing is written, no cursor is advanced, and no lease is needed: a reader
// that folds in memory cannot be the second writer L6 is about.
//
// At or before, never after. An event that happened later is not part of what was held at
// T, and letting one in is the same look-ahead bias that makes a backtest look brilliant
// and a liquidation arrive unannounced.
//
// The cost is the account's whole history per call, which is honest for M4 and is what
// position_snapshots exists to fix. Its invariant is
// snapshot(T) + events(T, T'] == FoldAt(T'), the same equality lineage already checks --
// so adding one must change this function's timing and never its answer.
func FoldAt(
	ctx context.Context, q *store.Queries, accountID uuid.UUID, at time.Time,
) (At, error) {
	integrations, err := q.ListAccountIntegrations(ctx, accountID)
	if err != nil {
		return At{}, fmt.Errorf("projection: read integrations for %s: %w", accountID, err)
	}

	out := At{
		Positions: map[PositionKey]position.State{},
		Balances:  map[BalanceKey]BalanceAt{},
	}
	lookup := newLegLookup(q)

	for _, integration := range integrations {
		if err := foldIntegrationAt(ctx, q, lookup, &out, accountID, integration.ID, at); err != nil {
			return At{}, err
		}
	}
	return out, nil
}

// foldIntegrationAt walks one integration's events in canonical order and stops at the
// first one past `at`. Stopping rather than filtering in SQL keeps this on the same read
// path the projector uses, so the two cannot drift in what "next event" means (L7).
func foldIntegrationAt(
	ctx context.Context,
	q *store.Queries,
	lookup *legLookup,
	out *At,
	accountID, integrationID uuid.UUID,
	at time.Time,
) error {
	states := map[int64]position.State{}
	balances := map[int64]balanceState{}
	touched := map[int64]bool{}
	var cursor ledger.Cursor

	for {
		events, err := ledger.Stream(ctx, q, accountID, integrationID, cursor, pageSize)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			break
		}

		done := false
		for _, e := range events {
			if e.EventTime.After(at) {
				done = true
				break
			}
			cursor = e.Cursor()
			out.Events++

			if err := applyBalance(ctx, lookup, balances, touched, e); err != nil {
				return err
			}
			if e.InstrumentID == nil {
				continue
			}
			next, err := position.Apply(states[*e.InstrumentID], e)
			if err != nil {
				return fmt.Errorf("projection: folding %s into instrument %d at %s: %w",
					e.VenueEventID, *e.InstrumentID, at.UTC().Format(time.RFC3339), err)
			}
			states[*e.InstrumentID] = next
		}
		if done || len(events) < pageSize {
			break
		}
	}

	for instrumentID, state := range states {
		out.Positions[PositionKey{integrationID, instrumentID}] = state
	}
	for assetID := range touched {
		s := balances[assetID]
		out.Balances[BalanceKey{integrationID, assetID}] = BalanceAt{
			Quantity: s.quantity, Cursor: s.cursor,
		}
	}
	return nil
}
