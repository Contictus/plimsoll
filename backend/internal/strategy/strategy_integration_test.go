//go:build integration

package strategy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/strategy"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

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

func seedAccount(t *testing.T) (accountID, integrationID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	accountID, integrationID = uuid.New(), uuid.New()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, email) VALUES ($1, $2)`,
			accountID, "strategy-"+accountID.String()+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'strategy-test')`, integrationID, accountID)
		return err
	}))
	return accountID, integrationID
}

func seedInstrument(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)

	var base, quote, id int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"SB-"+uuid.NewString()).Scan(&base))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'stablecoin') RETURNING id`,
		"SQ-"+uuid.NewString()).Scan(&quote))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id)
		 VALUES ($1, 'spot', $2, $3) RETURNING id`,
		"S-"+uuid.NewString(), base, quote).Scan(&id))
	return id
}

// foldOneFill puts a position in front of the tag, so the test tags something that exists.
func foldOneFill(t *testing.T, accountID, integrationID uuid.UUID, instrumentID int64) {
	t.Helper()
	ctx := context.Background()
	e := ledger.Event{
		AccountID:     accountID,
		IntegrationID: integrationID,
		VenueEventID:  fmt.Sprintf("spot:trade:%d:1", instrumentID),
		VenueSequence: 1,
		Source:        "rest",
		EventType:     ledger.TypeTrade,
		InstrumentID:  &instrumentID,
		Side:          ledger.SideBuy,
		Quantity:      decimal.NewNullDecimal(decimal.RequireFromString("2")),
		Price:         decimal.NewNullDecimal(decimal.RequireFromString("100")),
		EventTime:     time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
		Raw:           json.RawMessage(`{}`),
	}
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := ledger.Append(ctx, q, []ledger.Event{e})
		return err
	}))
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)
}

func newStrategy(t *testing.T, accountID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		var err error
		id, err = strategy.Create(ctx, q, accountID, name, strategy.KindBasis)
		return err
	}))
	return id
}

func tagsOf(t *testing.T, accountID uuid.UUID) map[strategy.PositionKey]strategy.Strategy {
	t.Helper()
	ctx := context.Background()
	var tagged map[strategy.PositionKey]strategy.Strategy
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		var err error
		tagged, err = strategy.Of(ctx, q, accountID)
		return err
	}))
	return tagged
}

// K30, AND THE REASON THE TAG IS NOT ON `positions`.
//
// A strategy assignment is something a human typed. `positions` is a projection: dropped and
// rebuilt from events (L3). A tag stored there is erased by every rebuild -- and the
// rebuild-equality test would still pass, because both sides would be equally empty. Nothing
// but this test stands between that design and a user re-tagging their book every time the
// fold is replayed.
func TestATagSurvivesAProjectionRebuild(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedAccount(t)
	instrumentID := seedInstrument(t)
	foldOneFill(t, accountID, integrationID, instrumentID)

	basis := newStrategy(t, accountID, "cash and carry")
	require.NoError(t, strategy.Assign(ctx, appPool(t), accountID,
		integrationID, instrumentID, &basis))

	require.NoError(t, projection.Rebuild(ctx, appPool(t), accountID, integrationID))

	key := strategy.PositionKey{IntegrationID: integrationID, InstrumentID: instrumentID}
	got, ok := tagsOf(t, accountID)[key]
	require.True(t, ok, "the rebuild erased the strategy tag")
	require.Equal(t, basis, got.ID)
	require.Equal(t, "cash and carry", got.Name)
}

// One strategy per position (K13's V1 scope). Re-assigning replaces; it does not accumulate,
// because two strategies claiming one position would double-count its exposure in whichever
// aggregate ran last.
func TestReassigningReplacesRatherThanAccumulates(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedAccount(t)
	instrumentID := seedInstrument(t)
	foldOneFill(t, accountID, integrationID, instrumentID)

	first := newStrategy(t, accountID, "directional")
	second := newStrategy(t, accountID, "basis")
	require.NoError(t, strategy.Assign(ctx, appPool(t), accountID, integrationID, instrumentID, &first))
	require.NoError(t, strategy.Assign(ctx, appPool(t), accountID, integrationID, instrumentID, &second))

	tagged := tagsOf(t, accountID)
	require.Len(t, tagged, 1)
	require.Equal(t, second,
		tagged[strategy.PositionKey{IntegrationID: integrationID, InstrumentID: instrumentID}].ID)
}

// Untagging is a thing a user does, and it must leave nothing behind.
func TestATagCanBeRemoved(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedAccount(t)
	instrumentID := seedInstrument(t)
	foldOneFill(t, accountID, integrationID, instrumentID)

	id := newStrategy(t, accountID, "directional")
	require.NoError(t, strategy.Assign(ctx, appPool(t), accountID, integrationID, instrumentID, &id))
	require.NoError(t, strategy.Assign(ctx, appPool(t), accountID, integrationID, instrumentID, nil))

	require.Empty(t, tagsOf(t, accountID))
}

// L12, with the application-level predicate deliberately absent: an unscoped SELECT under
// another account's context must return nothing. How a user groups their book is a thesis,
// and a thesis is exactly the kind of thing that must not leak.
func TestOneAccountCannotSeeAnothersTags(t *testing.T) {
	ctx := context.Background()
	accountA, integrationA := seedAccount(t)
	accountB, _ := seedAccount(t)
	instrumentID := seedInstrument(t)
	foldOneFill(t, accountA, integrationA, instrumentID)

	id := newStrategy(t, accountA, "private thesis")
	require.NoError(t, strategy.Assign(ctx, appPool(t), accountA, integrationA, instrumentID, &id))

	var tags, strategies int
	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), accountB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM position_strategies`).Scan(&tags); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM strategies`).Scan(&strategies)
	}))
	require.Zero(t, tags, "another account's tags were visible")
	require.Zero(t, strategies, "another account's strategy names were visible")
}
