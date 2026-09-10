//go:build integration

package collateral_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

func seedIntegration(t *testing.T) (accountID, integrationID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	accountID, integrationID = uuid.New(), uuid.New()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, email) VALUES ($1, $2)`,
			accountID, "collateral-"+accountID.String()+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'test')`, integrationID, accountID)
		return err
	}))
	return accountID, integrationID
}

func seedPerp(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)
	var base, quote int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"CB-"+uuid.NewString()).Scan(&base))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'stablecoin') RETURNING id`,
		"CQ-"+uuid.NewString()).Scan(&quote))

	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id, settle_asset_id)
		 VALUES ($1, 'perp', $2, $3, $3) RETURNING id`,
		"CP-"+uuid.NewString(), base, quote).Scan(&id))
	return id
}

func save(t *testing.T, accountID, integrationID uuid.UUID, s collateral.Snapshot) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		return collateral.Save(ctx, q, accountID, integrationID, s, time.Now().UTC())
	}))
}

func snapshotWith(instrumentID int64, at time.Time, positions ...collateral.PositionRisk) collateral.Snapshot {
	return collateral.Snapshot{
		AsOf:              at,
		MarginBalance:     d("10250"),
		WalletBalance:     d("10000"),
		UnrealizedPnL:     d("250"),
		MaintenanceMargin: d("1500"),
		AvailableBalance:  d("8000"),
		Positions:         positions,
		Brackets: map[string][]collateral.Bracket{"CP": {
			{Bracket: 1, NotionalFloor: d("0"), NotionalCap: d("50000"),
				MaintMarginRatio: d("0.004"), Cum: d("0")},
		}},
	}
}

func positionsOf(t *testing.T, accountID uuid.UUID) []store.ListCollateralPositionsRow {
	t.Helper()
	ctx := context.Background()
	var rows []store.ListCollateralPositionsRow
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		var err error
		rows, err = q.ListCollateralPositions(ctx, accountID)
		return err
	}))
	return rows
}

// The round trip, with every number arriving back with its digits intact (L1).
func TestASnapshotSurvivesTheRoundTrip(t *testing.T) {
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedPerp(t)
	at := time.Now().UTC().Truncate(time.Millisecond)

	save(t, accountID, integrationID, snapshotWith(instrumentID, at, collateral.PositionRisk{
		Symbol: "CP", InstrumentID: instrumentID,
		Quantity: d("0.5"), EntryPrice: d("60000"), MarkPrice: d("61000"),
		LiquidationPrice: d("48000"), Notional: d("30500"), Leverage: d("5"),
		MaintMargin: d("122"),
	}))

	ctx := context.Background()
	var got store.GetCollateralSnapshotRow
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		var err error
		got, err = q.GetCollateralSnapshot(ctx, store.GetCollateralSnapshotParams{
			AccountID: accountID, IntegrationID: integrationID,
		})
		return err
	}))
	require.Equal(t, at, got.AsOf.UTC())
	require.Equal(t, "10250", got.MarginBalance.String())
	require.Equal(t, "1500", got.MaintenanceMargin.String())

	rows := positionsOf(t, accountID)
	require.Len(t, rows, 1)
	require.Equal(t, "48000", rows[0].LiquidationPrice.String())
	require.Equal(t, "122", rows[0].MaintMargin.String())
}

// A position that closed between two captures must LEAVE. Merging instead of replacing
// leaves a closed position on the screen forever, still showing a liquidation distance --
// the most alarming possible way to be wrong, and the hardest to disbelieve, because every
// number on the row is real and only its existence is not.
func TestAClosedPositionDisappearsFromTheNextCapture(t *testing.T) {
	accountID, integrationID := seedIntegration(t)
	first, second := seedPerp(t), seedPerp(t)
	at := time.Now().UTC().Truncate(time.Millisecond)

	save(t, accountID, integrationID, snapshotWith(first, at,
		collateral.PositionRisk{Symbol: "CP", InstrumentID: first,
			Quantity: d("1"), EntryPrice: d("1"), MarkPrice: d("1"),
			LiquidationPrice: d("0.5"), Notional: d("1"), Leverage: d("1"), MaintMargin: d("0.004")},
		collateral.PositionRisk{Symbol: "CP2", InstrumentID: second,
			Quantity: d("2"), EntryPrice: d("1"), MarkPrice: d("1"),
			LiquidationPrice: d("0.5"), Notional: d("2"), Leverage: d("1"), MaintMargin: d("0.008")},
	))
	require.Len(t, positionsOf(t, accountID), 2)

	// The second capture sees only one of them: the other was closed.
	save(t, accountID, integrationID, snapshotWith(first, at.Add(time.Minute),
		collateral.PositionRisk{Symbol: "CP", InstrumentID: first,
			Quantity: d("1"), EntryPrice: d("1"), MarkPrice: d("1"),
			LiquidationPrice: d("0.5"), Notional: d("1"), Leverage: d("1"), MaintMargin: d("0.004")},
	))

	rows := positionsOf(t, accountID)
	require.Len(t, rows, 1, "a closed position survived the capture that no longer reports it")
	require.Equal(t, first, rows[0].InstrumentID)
}

// The brackets outlive the capture that fetched them. A capture asking about fewer symbols
// must not delete the tiers of a position it did not ask about: a stale bracket is a number
// M7.5 can still shock, and a missing one is an error it cannot answer through.
func TestBracketsSurviveACaptureThatDidNotMentionThem(t *testing.T) {
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedPerp(t)
	at := time.Now().UTC().Truncate(time.Millisecond)

	save(t, accountID, integrationID, snapshotWith(instrumentID, at,
		collateral.PositionRisk{Symbol: "CP", InstrumentID: instrumentID,
			Quantity: d("1"), EntryPrice: d("1"), MarkPrice: d("1"),
			LiquidationPrice: d("0.5"), Notional: d("1"), Leverage: d("1"), MaintMargin: d("0.004")},
	))

	// A later capture with no positions at all -- everything closed.
	save(t, accountID, integrationID, snapshotWith(instrumentID, at.Add(time.Minute)))
	require.Empty(t, positionsOf(t, accountID))

	ctx := context.Background()
	var brackets []store.ListLeverageBracketsRow
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		var err error
		brackets, err = q.ListLeverageBrackets(ctx, store.ListLeverageBracketsParams{
			AccountID: accountID, InstrumentID: instrumentID,
		})
		return err
	}))
	require.Len(t, brackets, 1, "the tier table was deleted with the position")
	require.Equal(t, "0.004", brackets[0].MaintMarginRatio.String())
}

// L12: another account's collateral is not visible, with the application-level predicate
// present AND with the RLS backstop underneath it. A margin buffer is among the most
// sensitive numbers this system holds.
func TestAnotherAccountsCollateralIsInvisible(t *testing.T) {
	accountA, integrationA := seedIntegration(t)
	accountB, _ := seedIntegration(t)
	instrumentID := seedPerp(t)
	at := time.Now().UTC().Truncate(time.Millisecond)

	save(t, accountA, integrationA, snapshotWith(instrumentID, at,
		collateral.PositionRisk{Symbol: "CP", InstrumentID: instrumentID,
			Quantity: d("1"), EntryPrice: d("1"), MarkPrice: d("1"),
			LiquidationPrice: d("0.5"), Notional: d("1"), Leverage: d("1"), MaintMargin: d("0.004")},
	))

	require.Empty(t, positionsOf(t, accountB))

	// And with no predicate at all, so anything returned is returned by RLS alone.
	ctx := context.Background()
	var count int
	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), accountB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM collateral_snapshots`).Scan(&count)
	}))
	require.Zero(t, count, "RLS alone did not hide another account's margin position")
}
