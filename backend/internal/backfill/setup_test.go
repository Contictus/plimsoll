//go:build integration

package backfill_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/backfill"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// These tests write real ledger events and never remove them: ledger_events has no DELETE
// policy and no DELETE grant, so nothing here has a path to (L2). Each test seeds its own
// account, so they do not see each other's rows.

// now is the instant every walk in these tests is run at. Fixed, because a backfill's
// window arithmetic is the thing under test and a moving clock would make the assertions
// about window boundaries depend on when the suite ran (L4 in spirit: time is an input).
var now = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func ownerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// seedIntegration creates an account and one integration under it, as the owner with the
// account bound -- FORCE ROW LEVEL SECURITY binds the owner too.
func seedIntegration(t *testing.T) backfill.Target {
	t.Helper()
	ctx := context.Background()
	target := backfill.Target{AccountID: uuid.New(), IntegrationID: uuid.New()}
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), target.AccountID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, email) VALUES ($1, $2)`,
			target.AccountID, "backfill-"+target.AccountID.String()+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'test')`, target.IntegrationID, target.AccountID)
		return err
	}))
	return target
}

// seedAsset inserts a canonical asset. The registry is reference data with no RLS and no
// write grant for the app role (00004), so this runs as the owner.
func seedAsset(t *testing.T) int64 {
	t.Helper()
	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"BF"+uuid.NewString()[:10],
	).Scan(&id))
	return id
}

func seedInstrument(t *testing.T) int64 {
	t.Helper()
	base, quote := seedAsset(t), seedAsset(t)
	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id)
		 VALUES ($1, 'spot', $2, $3) RETURNING id`,
		"BF-"+uuid.NewString()[:10], base, quote,
	).Scan(&id))
	return id
}

// newDeps wires the walk the way production does -- the app role, through tenancy.InTx --
// with the exchange and the registries faked. tradePageLimit is small so that a handful of
// trades still spans several pages, which is what the resume tests need.
func newDeps(t *testing.T, client *fakeClient, registry fakeRegistry) backfill.Deps {
	t.Helper()
	return backfill.Deps{
		DB:             appPool(t),
		Client:         client,
		Registry:       registry,
		Now:            func() time.Time { return now },
		TradePageLimit: 2,
	}
}

// events reads back everything the walk appended, in canonical order (L7).
func events(t *testing.T, target backfill.Target) []ledger.Event {
	t.Helper()
	ctx := context.Background()
	var got []ledger.Event
	require.NoError(t, tenancy.InTx(ctx, appPool(t), target.AccountID,
		func(q *store.Queries) error {
			var err error
			got, err = ledger.Stream(ctx, q,
				target.AccountID, target.IntegrationID, ledger.Cursor{}, 500)
			return err
		}))
	return got
}

func venueIDs(events []ledger.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.VenueEventID)
	}
	return out
}

func progressOf(t *testing.T, d backfill.Deps, target backfill.Target, scope string) backfill.Progress {
	t.Helper()
	p, err := backfill.Status(context.Background(), d, target, scope)
	require.NoError(t, err)
	return p
}

func id64(v int64) *int64 { return &v }

// forgetScope removes one progress row, which is what an operator forcing a fresh sweep
// does. It runs as the owner with the account bound, because FORCE ROW LEVEL SECURITY binds
// the owner too.
func forgetScope(t *testing.T, target backfill.Target, scope string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), target.AccountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM backfill_progress WHERE integration_id = $1 AND scope = $2`,
			target.IntegrationID, scope)
		return err
	}))
}
