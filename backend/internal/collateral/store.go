package collateral

import (
	"context"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
)

// Save writes one capture: the account totals, the positions, and the brackets for the
// symbols the account actually holds.
//
// The positions are replaced rather than merged. A position that closed between two captures
// has no row in the new one, and merging would leave it on the screen forever -- a closed
// position still showing a liquidation distance, which is the most alarming possible way to
// be wrong.
//
// The brackets are upserted rather than replaced, for the opposite reason: a capture that
// asked about fewer symbols must not delete the tiers of a position it did not ask about. A
// stale bracket is a number M7.5 can still shock; a missing one is an error it cannot answer
// through.
//
// q must come from tenancy.InTx, so all of this is one transaction and a partial capture
// never reaches a reader (L12).
func Save(
	ctx context.Context,
	q *store.Queries,
	accountID, integrationID uuid.UUID,
	s Snapshot,
	capturedAt time.Time,
) error {
	if err := q.UpsertCollateralSnapshot(ctx, store.UpsertCollateralSnapshotParams{
		AccountID:         accountID,
		IntegrationID:     integrationID,
		AsOf:              s.AsOf,
		CapturedAt:        capturedAt,
		MarginBalance:     s.MarginBalance,
		WalletBalance:     s.WalletBalance,
		UnrealizedPnl:     s.UnrealizedPnL,
		MaintenanceMargin: s.MaintenanceMargin,
		AvailableBalance:  s.AvailableBalance,
	}); err != nil {
		return fmt.Errorf("collateral: save snapshot for %s: %w", integrationID, err)
	}

	if err := q.DeleteCollateralPositions(ctx, store.DeleteCollateralPositionsParams{
		AccountID: accountID, IntegrationID: integrationID,
	}); err != nil {
		return fmt.Errorf("collateral: clear positions for %s: %w", integrationID, err)
	}

	for _, p := range s.Positions {
		if err := q.InsertCollateralPosition(ctx, store.InsertCollateralPositionParams{
			AccountID:        accountID,
			IntegrationID:    integrationID,
			InstrumentID:     p.InstrumentID,
			Quantity:         p.Quantity,
			EntryPrice:       p.EntryPrice,
			MarkPrice:        p.MarkPrice,
			LiquidationPrice: p.LiquidationPrice,
			Notional:         p.Notional,
			Leverage:         p.Leverage,
			MaintMargin:      p.MaintMargin,
		}); err != nil {
			return fmt.Errorf("collateral: save position %s (%s): %w", p.Symbol, integrationID, err)
		}

		for _, b := range s.Brackets[p.Symbol] {
			if err := q.UpsertLeverageBracket(ctx, store.UpsertLeverageBracketParams{
				AccountID:        accountID,
				IntegrationID:    integrationID,
				InstrumentID:     p.InstrumentID,
				Bracket:          int32(b.Bracket), //nolint:gosec // a venue tier number
				NotionalFloor:    b.NotionalFloor,
				NotionalCap:      b.NotionalCap,
				MaintMarginRatio: b.MaintMarginRatio,
				Cum:              b.Cum,
				CapturedAt:       capturedAt,
			}); err != nil {
				return fmt.Errorf("collateral: save bracket %d for %s: %w",
					b.Bracket, p.Symbol, err)
			}
		}
	}
	return nil
}
