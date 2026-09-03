package projection

import (
	"context"
	"errors"
	"fmt"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// loadCursor returns where the last run stopped. A missing row is the zero cursor, which
// sits before every event, so a first run and a rebuild take the same path.
func loadCursor(
	ctx context.Context, q *store.Queries, accountID, integrationID uuid.UUID,
) (ledger.Cursor, error) {
	row, err := q.GetProjectionCursor(ctx, store.GetProjectionCursorParams{
		AccountID: accountID, IntegrationID: integrationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Cursor{}, nil
	}
	if err != nil {
		return ledger.Cursor{}, fmt.Errorf("projection: read cursor for %s: %w", integrationID, err)
	}
	return ledger.Cursor{
		EventTime:     row.LastEventTime,
		VenueSequence: row.LastVenueSequence,
		VenueEventID:  row.LastVenueEventID,
	}, nil
}

// loadStates reads the whole projection for one integration back into engine states. It
// loads every instrument up front rather than lazily, because the count is bounded by what
// the account trades and one round trip beats one per instrument.
func loadStates(
	ctx context.Context, q *store.Queries, accountID, integrationID uuid.UUID,
) (map[int64]position.State, error) {
	rows, err := q.ListPositions(ctx, store.ListPositionsParams{
		AccountID: accountID, IntegrationID: integrationID,
	})
	if err != nil {
		return nil, fmt.Errorf("projection: read positions for %s: %w", integrationID, err)
	}

	states := make(map[int64]position.State, len(rows))
	for _, r := range rows {
		states[r.InstrumentID] = position.State{
			Quantity:      r.Quantity,
			AvgEntryPrice: r.AvgEntryPrice,
			RealizedPnL:   r.RealizedPnl,
			Cursor: ledger.Cursor{
				EventTime:     r.LastEventTime,
				VenueSequence: r.LastVenueSequence,
				VenueEventID:  r.LastVenueEventID,
			},
		}
	}

	fees, err := q.ListPositionFees(ctx, store.ListPositionFeesParams{
		AccountID: accountID, IntegrationID: integrationID,
	})
	if err != nil {
		return nil, fmt.Errorf("projection: read fees for %s: %w", integrationID, err)
	}
	// Ordered by (instrument_id, fee_asset) in SQL, so the slice is already in the stable
	// order the engine keeps it in and a resumed fold matches a fresh one exactly.
	for _, f := range fees {
		s := states[f.InstrumentID]
		s.Fees = append(s.Fees, position.FeeTotal{Asset: f.FeeAsset, Amount: f.Amount})
		states[f.InstrumentID] = s
	}
	return states, nil
}

// writePosition replaces one position row and its fee totals. The fees are deleted and
// reinserted rather than merged: the state holds cumulative totals, so replacing them is
// the whole truth and an asset that stopped appearing does not linger.
func writePosition(
	ctx context.Context,
	q *store.Queries,
	accountID, integrationID uuid.UUID,
	instrumentID int64,
	s position.State,
) error {
	if err := q.UpsertPosition(ctx, store.UpsertPositionParams{
		AccountID:         accountID,
		IntegrationID:     integrationID,
		InstrumentID:      instrumentID,
		Quantity:          s.Quantity,
		AvgEntryPrice:     s.AvgEntryPrice,
		RealizedPnl:       s.RealizedPnL,
		LastEventTime:     s.Cursor.EventTime,
		LastVenueSequence: s.Cursor.VenueSequence,
		LastVenueEventID:  s.Cursor.VenueEventID,
	}); err != nil {
		return fmt.Errorf("projection: write position %d: %w", instrumentID, err)
	}

	if err := q.DeletePositionFees(ctx, store.DeletePositionFeesParams{
		AccountID: accountID, IntegrationID: integrationID, InstrumentID: instrumentID,
	}); err != nil {
		return fmt.Errorf("projection: clear fees for instrument %d: %w", instrumentID, err)
	}
	for _, f := range s.Fees {
		if err := q.InsertPositionFee(ctx, store.InsertPositionFeeParams{
			AccountID:     accountID,
			IntegrationID: integrationID,
			InstrumentID:  instrumentID,
			FeeAsset:      f.Asset,
			Amount:        f.Amount,
		}); err != nil {
			return fmt.Errorf("projection: write %s fee for instrument %d: %w",
				f.Asset, instrumentID, err)
		}
	}
	return nil
}
