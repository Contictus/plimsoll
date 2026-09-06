package worker

import (
	"context"
	"fmt"

	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
)

// ledgerProjector folds one integration's ledger into its positions, under the same lease
// that lets this worker write the ledger at all.
type ledgerProjector struct {
	db                       tenancy.Beginner
	accountID, integrationID uuid.UUID
	ownerID                  string
}

// LedgerProjector is the production Projector: projection.Project, guarded by this
// worker's lease.
//
// The guard is not belt-and-braces. The projector advances a per-integration cursor, and
// the whole reason that cursor is safe is that exactly one process owns the integration
// (K20, L6). A worker whose lease lapsed between two ticks would otherwise fold and move
// the cursor underneath the worker that replaced it -- self-healing, because the row-count
// check would notice and rebuild, but self-healing after the fact is not the same as not
// happening (K37).
func LedgerProjector(
	db tenancy.Beginner, accountID, integrationID uuid.UUID, ownerID string,
) Projector {
	return ledgerProjector{db: db, accountID: accountID, integrationID: integrationID, ownerID: ownerID}
}

func (p ledgerProjector) Project(ctx context.Context) error {
	_, err := projection.Project(ctx, p.db, p.accountID, p.integrationID,
		projection.WithGuard(func(ctx context.Context, q *store.Queries) error {
			return GuardLease(ctx, q, p.accountID, p.integrationID, p.ownerID)
		}))
	if err != nil {
		return fmt.Errorf("worker: project %s: %w", p.integrationID, err)
	}
	return nil
}
