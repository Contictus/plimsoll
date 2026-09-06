package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/backfill"
	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/integration"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// leaseTTL is how long an integration stays claimed without a heartbeat. Long enough
	// that a slow transaction does not hand the integration away; short enough that a
	// worker killed without a chance to release does not strand it for long.
	leaseTTL = 2 * time.Minute

	// depositHistoryStart is how far back the deposit walk begins. Binance spot opened in
	// July 2017, so nothing can predate it and an earlier value costs empty windows and no
	// correctness.
	depositHistoryStart = "2017-07-01T00:00:00Z"

	// retryAfterLoss is the wait before trying to claim an integration again. Losing a
	// claim is the normal outcome on a fleet, so this is a poll interval rather than a
	// backoff: another worker holds it, and it will hold it until it stops.
	retryAfterLoss = 30 * time.Second
)

// deps is everything shared between the supervisors one process runs: one pool, one
// limiter, one symbol list. The limiter is shared on purpose -- Binance's limits are per IP,
// not per key, so a per-integration limiter alone would let one account's backfill get
// another account's IP banned (K24).
type deps struct {
	pool    *pgxpool.Pool
	keys    crypto.KeyProvider
	limiter binance.Limiter
	symbols []string
	restURL string
	wsURL   string
	ownerID string
	log     *slog.Logger
}

// supervise runs one integration for as long as the process lives, claiming it whenever it
// is free. It returns only when the context is cancelled: every other outcome -- another
// worker holds it, the connection dropped, the credential was rejected -- is something a
// later attempt may find changed.
func supervise(ctx context.Context, d deps, assignment worker.Assignment) {
	log := d.log.With(
		"integration_id", assignment.IntegrationID,
		"account_id", assignment.AccountID)

	for {
		err := runOnce(ctx, d, assignment)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, worker.ErrNotLeader):
			log.Debug("integration held by another worker")
		case errors.Is(err, worker.ErrLeaseLost):
			log.Warn("lost the lease; another worker has taken over")
		case err != nil:
			// Named without the credential (L13): the integration id is what an operator
			// needs to act, and it is the whole of what is safe to write down.
			log.Error("supervisor stopped", "error", err)
		default:
			log.Info("supervisor finished")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryAfterLoss):
		}
	}
}

// runOnce assembles one supervisor and runs it. The assembly happens per attempt rather
// than once, because the credential may have been rotated and the stream is not reusable
// after it closes.
func runOnce(ctx context.Context, d deps, assignment worker.Assignment) error {
	cred, err := loadCredential(ctx, d, assignment)
	if err != nil {
		return err
	}

	client, err := binance.New(binance.Config{
		IntegrationID: assignment.IntegrationID,
		Credential:    cred,
		Limiter:       d.limiter,
		BaseURL:       d.restURL,
	})
	if err != nil {
		return err
	}

	stream, err := binance.NewStream(binance.StreamConfig{
		IntegrationID: assignment.IntegrationID,
		Credential:    cred,
		URL:           d.wsURL,
		Limiter:       d.limiter,
	})
	if err != nil {
		return err
	}

	registry := worker.Registry{DB: d.pool, Exchange: exchangeName}
	target := backfill.Target{
		AccountID:     assignment.AccountID,
		IntegrationID: assignment.IntegrationID,
	}
	// The history walk yields to everything: it has hours to finish, and a live event is
	// something someone is waiting on (K24).
	backfillDeps := backfill.Deps{
		DB:       d.pool,
		Client:   client.WithPriority(ratelimitBackfill),
		Registry: registry,
		Now:      time.Now,
	}
	// The gap replay is not history: it is the window a live feed missed, so it is charged
	// at realtime priority and must not queue behind a backfill.
	resyncDeps := backfillDeps
	resyncDeps.Client = client

	since, err := time.Parse(time.RFC3339, depositHistoryStart)
	if err != nil {
		return err
	}

	supervisor, err := worker.NewSupervisor(worker.SupervisorConfig{
		DB:            d.pool,
		AccountID:     assignment.AccountID,
		IntegrationID: assignment.IntegrationID,
		OwnerID:       d.ownerID,
		LeaseTTL:      leaseTTL,
		Stream:        stream,
		Ingest: worker.StreamIngester{
			Resolver: registry,
			Context: binance.IngestContext{
				AccountID:     assignment.AccountID,
				IntegrationID: assignment.IntegrationID,
				Source:        binance.SourceStream,
			},
		},
		Resync: worker.WindowResyncer{
			Deps:    resyncDeps,
			Target:  target,
			Symbols: worker.TradedSymbols(resyncDeps, target),
		},
		Backfill: &backfill.Runner{
			Deps:    backfillDeps,
			Target:  target,
			Symbols: d.symbols,
			Since:   since,
		},
	})
	if err != nil {
		return err
	}
	return supervisor.Run(ctx)
}

// loadCredential reads the integration's key under its own account, through the same RLS
// path everything else uses. It never logs, returns or wraps the credential itself (L13).
func loadCredential(
	ctx context.Context, d deps, assignment worker.Assignment,
) (integration.Credential, error) {
	var cred integration.Credential
	err := tenancy.InTx(ctx, d.pool, assignment.AccountID, func(q *store.Queries) error {
		var err error
		cred, err = integration.LoadCredential(
			ctx, q, d.keys, assignment.AccountID, assignment.IntegrationID)
		return err
	})
	if err != nil {
		return integration.Credential{}, fmt.Errorf(
			"load credential for %s: %w", assignment.IntegrationID, err)
	}
	return cred, nil
}
