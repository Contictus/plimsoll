package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// valueEvery is how often a run is produced. Chosen from the reader's side: it is the
	// longest a portfolio total may lag the prices behind it before the lag costs more than
	// the rows saved. Alerts will evaluate on completed runs rather than per tick (M6), so
	// this is also the resolution risk is measured at.
	valueEvery = time.Minute

	// priceSource names the feed a run was built from, recorded on every run (K11).
	priceSource = "binance:spot"
)

// runValuations produces a valuation run on a ticker for as long as the process lives.
//
// It is NOT lease-guarded, unlike the ledger fold (K38), and the difference is worth
// stating. The fold advances a per-integration cursor, so two writers would interleave on
// one piece of state and lose events. A valuation run writes no cursor and belongs to no
// account: each one is a complete, self-contained snapshot, so a second producer costs
// duplicate rows and never a wrong number. Buying a lock to save rows would be paying in
// complexity for something storage does more cheaply.
func runValuations(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	pegs, err := loadPegs(ctx, pool)
	if err != nil {
		// Not fatal to the process. Without a peg set nothing can be priced, but the
		// ledger is still being filled -- and events are the half that cannot be recovered
		// later, while a valuation can be produced the moment the registry is curated.
		log.Warn("valuation runs not started", "error", err)
		return
	}

	ticker := time.NewTicker(valueEvery)
	defer ticker.Stop()
	log.Info("valuation runs started", "every", valueEvery, "peg_assets", len(pegs))

	for {
		if err := produceOne(ctx, pool, pegs); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Warned rather than fatal, for the same reason a failed fold is (K38): the
			// reader is told independently, because a run that never happened and a run
			// that is old look identical from outside and both raise price_stale.
			log.Warn("valuation run failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// produceOne writes one run in one transaction, so the run row and its prices commit
// together and a reader can never see a half-priced run.
func produceOne(ctx context.Context, pool *pgxpool.Pool, pegs valuation.PegSet) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin valuation run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := valuation.Produce(ctx, store.New(tx), priceSource, time.Now().UTC(), pegs); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit valuation run: %w", err)
	}
	return nil
}

// loadPegs reads the configured peg symbols. The resolution itself lives in the valuation
// package because the API rebuilds runs for `?at=` and must terminate its paths the same
// way: two readings of one setting is how the same instant comes to have two answers.
func loadPegs(ctx context.Context, pool *pgxpool.Pool) (valuation.PegSet, error) {
	return valuation.LoadPegs(ctx, store.New(pool),
		envOr("PLIMSOLL_PEG_ASSETS", valuation.DefaultPegAssets))
}
