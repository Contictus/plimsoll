// Reconciliation: the worker half of M7's comparison against the venue.

package worker

import (
	"context"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/events"
	"github.com/Contictus/plimsoll/backend/internal/reconciliation"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
)

// ReconcileConfig is one integration's reconciliation, assembled.
type ReconcileConfig struct {
	Deps    reconciliation.Deps
	OwnerID string
	Now     func() time.Time
}

// ReconcileRunner builds the comparison the supervisor runs on its tick.
//
// The lease is checked before the venue is asked, not after. A worker that has lost the
// integration must not spend its weight budget describing something another worker is already
// keeping current -- and must certainly not write findings about a fold it no longer owns.
func ReconcileRunner(cfg ReconcileConfig) Reconciler { return reconcileRunner{cfg: cfg} }

type reconcileRunner struct{ cfg ReconcileConfig }

func (r reconcileRunner) Reconcile(ctx context.Context) error {
	now := r.cfg.Now
	if now == nil {
		now = time.Now
	}

	if err := tenancy.InTx(ctx, r.cfg.Deps.DB, r.cfg.Deps.AccountID,
		func(q *store.Queries) error {
			return GuardLease(ctx, q, r.cfg.Deps.AccountID, r.cfg.Deps.IntegrationID, r.cfg.OwnerID)
		}); err != nil {
		return err
	}

	if err := reconciliation.Run(ctx, r.cfg.Deps, now().UTC()); err != nil {
		return err
	}

	// The register moved, so a page showing it is told to re-read. A separate transaction
	// from the run's own on purpose: the findings are already durable by the time this is
	// sent, so a failed hint costs a refresh and never a wrong number (K51).
	return tenancy.InTx(ctx, r.cfg.Deps.DB, r.cfg.Deps.AccountID, func(q *store.Queries) error {
		return events.Publish(ctx, q, r.cfg.Deps.AccountID, events.TopicPortfolio)
	})
}
