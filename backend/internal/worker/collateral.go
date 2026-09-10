// Collateral capture: the worker half of M5's margin picture.

package worker

import (
	"context"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
)

// captureTolerance is how far apart the two halves of a capture may be before it is refused
// as torn. The account call and the positionRisk call are made back to back, so anything
// beyond this is not latency -- it is one of them having waited on a rate limit, and a
// margin buffer paired with a liquidation price from a different second is one screen
// describing two accounts (F14).
const captureTolerance = 2 * time.Second

// CaptureConfig is one integration's margin capture, assembled.
type CaptureConfig struct {
	DB                       tenancy.Beginner
	AccountID, IntegrationID uuid.UUID
	OwnerID                  string

	Source   collateral.Source
	Resolver collateral.InstrumentResolver
	Now      func() time.Time
}

// CollateralCapturer builds the capture the supervisor runs on its tick: ask the venue,
// then store the answer under the lease that makes this worker the writer.
//
// The snapshot is stored whole or not at all. A capture that saved its totals and failed
// before its positions would leave a margin buffer beside the previous tick's liquidation
// prices -- one screen describing two moments, which is the tearing NewSnapshot refuses at
// the other end of the same call.
func CollateralCapturer(cfg CaptureConfig) Capturer { return captureRunner{cfg: cfg} }

type captureRunner struct{ cfg CaptureConfig }

func (r captureRunner) Capture(ctx context.Context) error {
	now := r.cfg.Now
	if now == nil {
		now = time.Now
	}

	snapshot, err := collateral.Capture(ctx, r.cfg.Source, r.cfg.Resolver, now, captureTolerance)
	if err != nil {
		return err
	}

	return tenancy.InTx(ctx, r.cfg.DB, r.cfg.AccountID, func(q *store.Queries) error {
		// The same guard every other write this worker makes carries: a worker that lost
		// the integration stops describing it, rather than overwriting the picture the
		// worker that now holds it is keeping current.
		if err := GuardLease(ctx, q, r.cfg.AccountID, r.cfg.IntegrationID, r.cfg.OwnerID); err != nil {
			return err
		}
		return collateral.Save(ctx, q, r.cfg.AccountID, r.cfg.IntegrationID, snapshot, now().UTC())
	})
}
