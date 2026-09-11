package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/transfer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// transferMatchEvery is how often the unlinked legs are re-matched.
//
// Five minutes rather than seconds because the thing it waits for is a deposit crediting on
// another venue, which takes minutes to hours. Matching faster would spend reads to discover
// the same unmatched leg over and over, and the queue it feeds is read by a human anyway.
const transferMatchEvery = 5 * time.Minute

// runTransferMatching joins the two halves of every account's cross-venue movements.
//
// It writes links and findings; it writes no ledger row and changes no balance. A failure is
// logged and the pass is retried: an unmatched leg stays in the register saying what it is
// mistaken for, so the user is told about the condition whether or not this loop is healthy
// (L11, K57).
func runTransferMatching(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	ticker := time.NewTicker(transferMatchEvery)
	defer ticker.Stop()
	log.Info("cross-venue transfer matching started", "every", transferMatchEvery)

	for {
		accounts, err := accountsWithTransferLegs(ctx, pool)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("could not list accounts with transfer legs", "error", err)
		}
		for _, accountID := range accounts {
			result, err := transfer.Reconcile(
				ctx, pool, accountID, transfer.DefaultRules, time.Now().UTC())
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warn("transfer matching failed", "account_id", accountID, "error", err)
				continue
			}
			if len(result.Links) > 0 {
				log.Info("cross-venue transfers joined",
					"account_id", accountID, "links", len(result.Links),
					"unmatched", len(result.UnmatchedOut)+len(result.UnmatchedIn))
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// accountsWithTransferLegs asks once for the accounts that have anything to match. An account
// with a single venue has no cross-venue movement by definition, so the query requires two --
// which is what keeps this loop free for every account that is not the case it exists for.
func accountsWithTransferLegs(ctx context.Context, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	rows, err := pool.Query(ctx,
		`SELECT account_id
		   FROM ledger_events
		  WHERE event_type IN ('DEPOSIT', 'WITHDRAWAL')
		  GROUP BY account_id
		 HAVING count(DISTINCT integration_id) > 1`)
	if err != nil {
		return nil, fmt.Errorf("list accounts with transfer legs: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
