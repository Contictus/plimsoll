package projection

import (
	"context"
	"errors"
	"fmt"

	"github.com/Contictus/plimsoll/backend/internal/balance"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// balanceState is one asset's running total and where it got to. The cursor is per asset
// for the same reason positions keep one per instrument: a rebuild has to be able to say
// which event each row is current as of.
type balanceState struct {
	quantity decimal.Decimal
	cursor   ledger.Cursor
}

// legLookup caches instrument legs for the length of one fold. An account trades few
// instruments and folds many events over them, so this is one read per instrument rather
// than one per fill -- and it is a cache of immutable reference data, which is the only
// kind that is safe to hold without an invalidation story.
type legLookup struct {
	q     *store.Queries
	known map[int64]balance.Legs
}

func newLegLookup(q *store.Queries) *legLookup {
	return &legLookup{q: q, known: map[int64]balance.Legs{}}
}

func (l *legLookup) legs(ctx context.Context, instrumentID int64) (*balance.Legs, error) {
	if got, ok := l.known[instrumentID]; ok {
		return &got, nil
	}
	row, err := l.q.GetInstrumentLegs(ctx, instrumentID)
	if err != nil {
		return nil, fmt.Errorf("projection: read legs of instrument %d: %w", instrumentID, err)
	}
	legs := balance.Legs{BaseAssetID: row.BaseAssetID, QuoteAssetID: row.QuoteAssetID}
	l.known[instrumentID] = legs
	return &legs, nil
}

// loadBalances reads the projection back into engine state so a resumed fold continues
// rather than restarts.
func loadBalances(
	ctx context.Context, q *store.Queries, accountID, integrationID uuid.UUID,
) (map[int64]balanceState, error) {
	rows, err := q.ListAssetBalances(ctx, store.ListAssetBalancesParams{
		AccountID: accountID, IntegrationID: integrationID,
	})
	if err != nil {
		return nil, fmt.Errorf("projection: read balances for %s: %w", integrationID, err)
	}
	out := make(map[int64]balanceState, len(rows))
	for _, r := range rows {
		out[r.AssetID] = balanceState{
			quantity: r.Quantity,
			cursor: ledger.Cursor{
				EventTime:     r.LastEventTime,
				VenueSequence: r.LastVenueSequence,
				VenueEventID:  r.LastVenueEventID,
			},
		}
	}
	return out, nil
}

// applyBalance folds one event into the running balances.
//
// When the event paid a fee whose asset never resolved, the engine refuses the whole event
// rather than guess -- and this is where that refusal is turned into a decision. The fill
// is kept and the fee's balance effect is dropped, because losing a fill to keep a fee is
// the worse trade. The shortfall is exact, it is the fee, and the reader is told about it
// through unknown_symbol rather than left to find it in a reconciliation (K22, L11).
func applyBalance(
	ctx context.Context,
	lookup *legLookup,
	states map[int64]balanceState,
	touched map[int64]bool,
	e ledger.Event,
) error {
	resolved := balance.Resolved{FeeAssetID: e.FeeAssetID}
	if e.InstrumentID != nil {
		legs, err := lookup.legs(ctx, *e.InstrumentID)
		if err != nil {
			return err
		}
		resolved.Legs = legs
	}

	deltas, err := balance.Deltas(e, resolved)
	if errors.Is(err, balance.ErrUnresolved) && e.Fee.Valid && e.FeeAssetID == nil {
		withoutFee := e
		withoutFee.Fee = decimal.NullDecimal{}
		deltas, err = balance.Deltas(withoutFee, resolved)
	}
	if err != nil {
		return fmt.Errorf("projection: balance fold of %s: %w", e.VenueEventID, err)
	}

	for _, d := range deltas {
		state := states[d.AssetID]
		state.quantity = state.quantity.Add(d.Amount)
		state.cursor = e.Cursor()
		states[d.AssetID] = state
		touched[d.AssetID] = true
	}
	return nil
}

// writeBalances replaces every balance the fold moved.
func writeBalances(
	ctx context.Context,
	q *store.Queries,
	accountID, integrationID uuid.UUID,
	states map[int64]balanceState,
	touched map[int64]bool,
) error {
	for assetID := range touched {
		s := states[assetID]
		if err := q.UpsertAssetBalance(ctx, store.UpsertAssetBalanceParams{
			AccountID:         accountID,
			IntegrationID:     integrationID,
			AssetID:           assetID,
			Quantity:          s.quantity,
			LastEventTime:     s.cursor.EventTime,
			LastVenueSequence: s.cursor.VenueSequence,
			LastVenueEventID:  s.cursor.VenueEventID,
		}); err != nil {
			return fmt.Errorf("projection: write balance of asset %d: %w", assetID, err)
		}
	}
	return nil
}
