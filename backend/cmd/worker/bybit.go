package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/backfill"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/exchange/bybit"
	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/worker"
	"github.com/google/uuid"
)

const (
	// bybitHistoryStart is how far back the walks begin. Bybit opened in 2018, so nothing
	// can predate it and an earlier value costs empty windows and no correctness.
	bybitHistoryStart = "2018-01-01T00:00:00Z"

	// bybitWalkEvery is how often the history is re-walked.
	//
	// Deposits and withdrawals keep arriving, so a walk is never finished the way a trade
	// history is -- it is "walked up to here", and the next run continues. Ten minutes,
	// because a deposit crediting on a venue takes minutes to hours and the overlap between
	// windows makes a re-walk cheap rather than free (B3, L5).
	bybitWalkEvery = 10 * time.Minute
)

// superviseBybit runs one Bybit integration.
//
// It is a much smaller supervisor than the Binance one and that is not an omission: this
// milestone connects Bybit for the half of it that feeds cross-venue matching -- deposits and
// withdrawals. Trades, positions and funding are a later milestone, and half-building them
// here is how a venue ends up with a normalizer nothing calls (K38).
//
// There is no stream. Bybit's user data feed is not wrapped, so history is walked on a ticker
// and the reader is told how current it is by the ordinary ingest state (K39).
func superviseBybit(ctx context.Context, d deps, assignment worker.Assignment) {
	log := d.log.With(
		"integration_id", assignment.IntegrationID,
		"account_id", assignment.AccountID,
		"exchange", "bybit")

	for {
		if err := runBybitOnce(ctx, d, assignment); err != nil {
			switch {
			case ctx.Err() != nil:
				return
			case errors.Is(err, worker.ErrNotLeader):
				log.Debug("integration held by another worker")
			case errors.Is(err, worker.ErrLeaseLost):
				log.Warn("lost the lease; another worker has taken over")
			default:
				// The integration id is what an operator needs to act, and it is the whole
				// of what is safe to write down (L13).
				log.Error("bybit supervisor stopped", "error", err)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryAfterLoss):
		}
	}
}

// runBybitOnce claims the integration and walks its history until the lease is lost or the
// process stops.
func runBybitOnce(ctx context.Context, d deps, assignment worker.Assignment) error {
	cred, err := loadCredential(ctx, d, assignment)
	if err != nil {
		return err
	}

	client, err := bybit.New(bybit.Config{
		IntegrationID: assignment.IntegrationID,
		Credential:    cred,
		Limiter:       bybitLimiter{d.limiter},
		BaseURL:       d.bybitURL,
	})
	if err != nil {
		return err
	}

	// The key is verified before anything is read from it. K9's gate is at connect time, and
	// this is the second place it matters: a key that gained a permission since it was
	// connected must not be used, and the venue is the only thing that knows.
	info, err := client.QueryAPI(ctx)
	if err != nil {
		return fmt.Errorf("bybit: read key permissions: %w", err)
	}
	if _, err := bybit.ParsePermissions(info); err != nil {
		return err
	}

	claimed, err := worker.Claim(ctx, d.pool, assignment.AccountID, assignment.IntegrationID,
		d.ownerID, leaseTTL)
	if err != nil {
		return err
	}
	if !claimed {
		return worker.ErrNotLeader
	}

	since, err := time.Parse(time.RFC3339, bybitHistoryStart)
	if err != nil {
		return err
	}

	// Patient priority: the shared per-IP budget belongs to whatever is live, and a history
	// walk is the thing that can wait (K24).
	deps := backfill.BybitDeps{
		DB:       d.pool,
		Source:   client.WithPriority(ratelimit.PriorityBackfill),
		Registry: worker.Registry{DB: d.pool, Exchange: "bybit"},
		Now:      func() time.Time { return time.Now().UTC() },
	}
	target := backfill.Target{
		AccountID: assignment.AccountID, IntegrationID: assignment.IntegrationID,
	}

	ticker := time.NewTicker(bybitWalkEvery)
	defer ticker.Stop()
	for {
		if err := renewOrFail(ctx, d, assignment); err != nil {
			return err
		}
		if err := backfill.WalkBybitDeposits(ctx, deps, target, since); err != nil {
			return err
		}
		if err := backfill.WalkBybitWithdrawals(ctx, deps, target, since); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// bybitLimiter adapts the shared limiter's argument order to this package's interface.
//
// One limiter for both venues, deliberately: the per-IP budget belongs to the address and not
// to the key, so a second limiter would be a second budget for requests leaving one machine --
// which is the mistake K24 exists to prevent, arriving through a second venue.
type bybitLimiter struct{ inner binance.Limiter }

func (l bybitLimiter) Acquire(
	ctx context.Context, integrationID uuid.UUID, weight int, p ratelimit.Priority,
) error {
	return l.inner.Acquire(ctx, integrationID, p, weight)
}

// renewOrFail keeps the lease, and gives it up rather than writing without it. A worker that
// lost the integration must stop describing it (K20, K37).
func renewOrFail(ctx context.Context, d deps, assignment worker.Assignment) error {
	return tenancy.InTx(ctx, d.pool, assignment.AccountID, func(q *store.Queries) error {
		return worker.GuardLease(ctx, q,
			assignment.AccountID, assignment.IntegrationID, d.ownerID)
	})
}
