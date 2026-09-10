package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/alert"
	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// alertEvery is how often every account's rules are evaluated. It follows the valuation
	// interval rather than the market: alerts evaluate on completed runs (ARCHITECTURE
	// section 7), so evaluating more often than runs are produced would only re-read the
	// same run and re-decide the same thing.
	alertEvery = time.Minute

	// deliveryTimeout bounds one channel's attempt. A channel that hangs must not hold up
	// the accounts behind it in the same pass.
	deliveryTimeout = 10 * time.Second

	// priceTTL and collateralTTL are the API's own tolerances, duplicated here rather than
	// shared because the two processes are deployed separately. The consequence of drift is
	// a reason raised a little sooner or later in a message, never a wrong number.
	priceTTL      = 20 * time.Minute
	collateralTTL = 2 * time.Minute
)

// runAlerts evaluates every account's rules on a ticker.
//
// One loop for all accounts rather than one per integration: rules are written about an
// account's portfolio, and the leverage of a book spread over two exchanges is not two
// numbers. It holds no lease for the same reason -- there is nothing here another worker
// could corrupt by also doing it, and the alert state's own transaction is what keeps two
// passes from announcing the same condition twice.
func runAlerts(ctx context.Context, pool *pgxpool.Pool, keys crypto.KeyProvider, log *slog.Logger) {
	client := &http.Client{Timeout: deliveryTimeout}
	ticker := time.NewTicker(alertEvery)
	defer ticker.Stop()
	log.Info("alert evaluation started", "every", alertEvery)

	for {
		accounts, err := accountsWithRules(ctx, pool)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("could not list accounts to alert on", "error", err)
		}
		for _, accountID := range accounts {
			if err := alertOne(ctx, pool, keys, client, accountID); err != nil {
				if ctx.Err() != nil {
					return
				}
				// Named by account and never by rule content or channel: a warning about
				// alerting must not become the place a channel secret is written down (L13).
				log.Warn("alert evaluation failed", "account_id", accountID, "error", err)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// accountsWithRules asks once, as the app role, for the accounts that have anything to
// evaluate. Accounts with no rules cost nothing, which is what lets this be one loop.
func accountsWithRules(ctx context.Context, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT account_id FROM alert_rules WHERE enabled`)
	if err != nil {
		return nil, fmt.Errorf("list accounts with alert rules: %w", err)
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

// alertOne evaluates one account and delivers what it found.
//
// The channels are decrypted immediately before the pass and dropped after it: a decrypted
// token's lifetime should be one evaluation rather than the process's (K25).
func alertOne(
	ctx context.Context, pool *pgxpool.Pool, keys crypto.KeyProvider,
	client *http.Client, accountID uuid.UUID,
) error {
	var channels []alert.Deliverer
	if err := tenancy.InTx(ctx, pool, accountID, func(q *store.Queries) error {
		var err error
		channels, err = alert.Deliverers(ctx, q, keys, accountID, client)
		return err
	}); err != nil {
		return err
	}

	_, err := alert.Run(ctx, alert.RunDeps{
		DB:        pool,
		AccountID: accountID,
		Window: portfolio.Window{
			Now: time.Now().UTC(), LeaseTTL: leaseTTL, PriceTTL: priceTTL,
		},
		CollateralTTL: collateralTTL,
		Channels:      channels,
	})
	return err
}
