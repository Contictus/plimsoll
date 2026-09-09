package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

const (
	// valueEvery is how often a run is produced. Chosen from the reader's side: it is the
	// longest a portfolio total may lag the prices behind it before the lag costs more than
	// the rows saved. Alerts will evaluate on completed runs rather than per tick (M6), so
	// this is also the resolution risk is measured at.
	valueEvery = time.Minute

	// priceSource names the feed a run was built from, recorded on every run (K11).
	priceSource = "binance:spot"

	// defaultPegAssets is what terminates every price path. One asset, not two: the
	// smaller the set, the more is priced through a real market instead of assumed (K17).
	// USDT is deliberately absent, so a USDT depeg shows up in the numbers rather than
	// being assumed away.
	defaultPegAssets = "USDC"
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

// loadPegs resolves the configured peg symbols to asset ids.
//
// By symbol rather than by id, because configuration names assets the way a human does and
// an id is a database detail that differs between deployments. A symbol that does not
// resolve is an error rather than a skip: silently starting with an empty peg set would
// make every asset unpriceable and the cause invisible.
func loadPegs(ctx context.Context, pool *pgxpool.Pool) (valuation.PegSet, error) {
	configured := envOr("PLIMSOLL_PEG_ASSETS", defaultPegAssets)
	one := decimal.NewFromInt(1)

	pegs := valuation.PegSet{}
	q := store.New(pool)
	for _, symbol := range strings.Split(configured, ",") {
		symbol = strings.TrimSpace(strings.ToUpper(symbol))
		if symbol == "" {
			continue
		}
		id, err := q.GetAssetIDBySymbol(ctx, symbol)
		if err != nil {
			return nil, fmt.Errorf("peg asset %q is not in the registry: %w", symbol, err)
		}
		pegs[id] = one
	}
	if len(pegs) == 0 {
		return nil, errors.New("no peg asset is configured; nothing could terminate a price path")
	}
	return pegs, nil
}
