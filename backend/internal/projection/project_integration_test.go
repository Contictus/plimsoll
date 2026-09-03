//go:build integration

package projection_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Like the ledger tests, these leave their events behind: nothing here has a path to
// delete one (L2). The projections they build are droppable by design -- that is what
// Rebuild does, and what L3 is about.

func amount(s string) decimal.NullDecimal {
	return decimal.NewNullDecimal(decimal.RequireFromString(s))
}

var epoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// trade mirrors the builder in the engine's unit tests. Duplicated rather than exported,
// because a test helper in production code is a door nobody meant to leave open.
func trade(side ledger.Side, quantity, price string, seq int64) ledger.Event {
	return ledger.Event{
		VenueEventID:  fmt.Sprintf("usdm:trade:BTCUSDT:%d", seq),
		VenueSequence: seq,
		Source:        "rest",
		EventType:     ledger.TypeTrade,
		Side:          side,
		Quantity:      amount(quantity),
		Price:         amount(price),
		EventTime:     epoch.Add(time.Duration(seq) * time.Second),
	}
}

func withFee(e ledger.Event, fee, asset string) ledger.Event {
	e.Fee = amount(fee)
	e.FeeAsset = asset
	return e
}

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
			accountID, "position-"+accountID.String()+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'test')`, integrationID, accountID)
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
		"PB-"+uuid.NewString()).Scan(&base))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"PQ-"+uuid.NewString()).Scan(&quote))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id,
		                          settle_asset_id)
		 VALUES ($1, 'perp', $2, $3, $3) RETURNING id`,
		"P-"+uuid.NewString(), base, quote).Scan(&id))
	return id
}

// storable turns a unit-test event into one the ledger will accept: tenancy, an
// instrument, and a raw payload.
func storable(e ledger.Event, accountID, integrationID uuid.UUID, instrumentID int64) ledger.Event {
	e.AccountID = accountID
	e.IntegrationID = integrationID
	e.InstrumentID = &instrumentID
	e.Raw = json.RawMessage(`{"probe":"projection"}`)
	return e
}

func appendEvents(t *testing.T, accountID uuid.UUID, events ...ledger.Event) {
	t.Helper()
	require.NoError(t, tenancy.InTx(context.Background(), appPool(t), accountID,
		func(q *store.Queries) error {
			inserted, err := ledger.Append(context.Background(), q, events)
			if err == nil {
				require.Equal(t, len(events), inserted)
			}
			return err
		}))
}

func project(t *testing.T, accountID, integrationID uuid.UUID) {
	t.Helper()
	_, err := projection.Project(context.Background(), appPool(t), accountID, integrationID)
	require.NoError(t, err)
}

// projectedRow is every column of the projection that carries meaning. updated_at is
// deliberately absent: it records when the row was written, not what the fold computed,
// and an incremental projection and a rebuild legitimately differ on it.
type projectedRow struct {
	InstrumentID      int64
	Quantity          decimal.Decimal
	AvgEntryPrice     decimal.Decimal
	RealizedPnL       decimal.Decimal
	LastEventTime     time.Time
	LastVenueSequence int64
	LastVenueEventID  string
	Fees              []position.FeeTotal
}

// compareExactly compares decimals through their text, so the assertion is about the
// digits stored rather than about numeric equality. 100 and 100.000 are equal numbers and
// different rows; L3 asks for the second kind of sameness.
var compareExactly = cmp.Options{
	cmp.Transformer("decimal", func(d decimal.Decimal) string { return d.String() }),
	cmp.Comparer(func(a, b time.Time) bool { return a.Equal(b) }),
}

func snapshot(t *testing.T, accountID, integrationID uuid.UUID) []projectedRow {
	t.Helper()
	ctx := context.Background()
	var rows []projectedRow

	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), accountID, func(tx pgx.Tx) error {
		result, err := tx.Query(ctx,
			`SELECT instrument_id, quantity, avg_entry_price, realized_pnl,
			        last_event_time, last_venue_sequence, last_venue_event_id
			 FROM positions WHERE integration_id = $1 ORDER BY instrument_id`, integrationID)
		if err != nil {
			return err
		}
		defer result.Close()
		for result.Next() {
			var r projectedRow
			if err := result.Scan(&r.InstrumentID, &r.Quantity, &r.AvgEntryPrice,
				&r.RealizedPnL, &r.LastEventTime, &r.LastVenueSequence,
				&r.LastVenueEventID); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		if err := result.Err(); err != nil {
			return err
		}

		for i := range rows {
			fees, err := tx.Query(ctx,
				`SELECT fee_asset, amount FROM position_fees
				 WHERE integration_id = $1 AND instrument_id = $2 ORDER BY fee_asset`,
				integrationID, rows[i].InstrumentID)
			if err != nil {
				return err
			}
			for fees.Next() {
				var f position.FeeTotal
				if err := fees.Scan(&f.Asset, &f.Amount); err != nil {
					fees.Close()
					return err
				}
				rows[i].Fees = append(rows[i].Fees, f)
			}
			fees.Close()
			if err := fees.Err(); err != nil {
				return err
			}
		}
		return nil
	}))
	return rows
}

// INVARIANT 1 (ARCHITECTURE.md §12): projecting the same events twice changes nothing.
// The second run must find its cursor where it left it and do no work, rather than fold
// every event again and double the position.
func TestProjectingTwiceChangesNothing(t *testing.T) {
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedInstrument(t)

	appendEvents(t, accountID,
		storable(trade(ledger.SideBuy, "2", "100", 1), accountID, integrationID, instrumentID),
		storable(withFee(trade(ledger.SideSell, "1", "150", 2), "0.2", "USDT"),
			accountID, integrationID, instrumentID),
	)

	project(t, accountID, integrationID)
	once := snapshot(t, accountID, integrationID)

	project(t, accountID, integrationID)
	twice := snapshot(t, accountID, integrationID)

	require.Len(t, once, 1)
	require.Equal(t, "1", once[0].Quantity.String())
	require.Equal(t, "100", once[0].AvgEntryPrice.String())
	require.Equal(t, "50", once[0].RealizedPnL.String())
	require.Empty(t, cmp.Diff(once, twice, compareExactly))
}

// INVARIANT 2: the order events were ingested in must not reach the projection. The fold
// reads in canonical order (L7), so a backfill that arrives newest-first and a stream that
// arrives oldest-first produce the same position -- which is the whole reason
// avg_entry_price is allowed to depend on order at all.
func TestIngestOrderDoesNotChangeTheProjection(t *testing.T) {
	forward, forwardIntegration := seedIntegration(t)
	scrambled, scrambledIntegration := seedIntegration(t)
	instrumentID := seedInstrument(t)

	// Three fills, two of them sharing an event_time so the tiebreak is exercised too.
	build := func(accountID, integrationID uuid.UUID) []ledger.Event {
		first := trade(ledger.SideBuy, "1", "100", 1)
		tied := trade(ledger.SideBuy, "3", "200", 1)
		tied.VenueEventID = first.VenueEventID + ":b"
		last := withFee(trade(ledger.SideSell, "2", "300", 2), "0.5", "BNB")
		return []ledger.Event{
			storable(first, accountID, integrationID, instrumentID),
			storable(tied, accountID, integrationID, instrumentID),
			storable(last, accountID, integrationID, instrumentID),
		}
	}

	inOrder := build(forward, forwardIntegration)
	appendEvents(t, forward, inOrder...)

	reversed := build(scrambled, scrambledIntegration)
	appendEvents(t, scrambled, reversed[2], reversed[0], reversed[1])

	project(t, forward, forwardIntegration)
	project(t, scrambled, scrambledIntegration)

	require.Empty(t, cmp.Diff(
		snapshot(t, forward, forwardIntegration),
		snapshot(t, scrambled, scrambledIntegration),
		compareExactly,
	), "the ingest order reached the projection")
}

// INVARIANT 3, and the one that is never skipped (L3): drop the projection, fold the
// ledger from zero, and every field must match what the incremental run produced. There is
// no acceptable delta -- a projection that cannot be rebuilt is a second source of truth.
func TestRebuildingFromZeroReproducesTheIncrementalProjection(t *testing.T) {
	accountID, integrationID := seedIntegration(t)
	first, second := seedInstrument(t), seedInstrument(t)

	// Projected in two passes with events arriving in between, so the incremental side of
	// the comparison genuinely used its cursor rather than folding everything at once.
	appendEvents(t, accountID,
		storable(trade(ledger.SideBuy, "2", "100", 1), accountID, integrationID, first),
		storable(withFee(trade(ledger.SideBuy, "1", "50", 2), "0.01", "BNB"),
			accountID, integrationID, second),
	)
	project(t, accountID, integrationID)

	appendEvents(t, accountID,
		storable(trade(ledger.SideSell, "3", "120", 3), accountID, integrationID, first),
		storable(withFee(trade(ledger.SideSell, "1", "40", 4), "0.02", "BNB"),
			accountID, integrationID, second),
	)
	project(t, accountID, integrationID)

	incremental := snapshot(t, accountID, integrationID)
	require.Len(t, incremental, 2)

	require.NoError(t,
		projection.Rebuild(context.Background(), appPool(t), accountID, integrationID))
	rebuilt := snapshot(t, accountID, integrationID)

	require.Empty(t, cmp.Diff(incremental, rebuilt, compareExactly),
		"the projection cannot be rebuilt from the ledger")
}

// INVARIANT 4 (D2): a strategy assignment is user input, not a fold output, so it must
// survive the projection being dropped and rebuilt. Documented as a column on positions,
// it would have been erased here -- and the rebuild-equality test above would still have
// passed, because both sides would have been equally empty.
func TestAStrategyAssignmentSurvivesARebuild(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedInstrument(t)
	strategyID := uuid.New()

	appendEvents(t, accountID,
		storable(trade(ledger.SideBuy, "1", "100", 1), accountID, integrationID, instrumentID))
	project(t, accountID, integrationID)

	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO position_strategies
			   (account_id, integration_id, instrument_id, strategy_id)
			 VALUES ($1, $2, $3, $4)`, accountID, integrationID, instrumentID, strategyID)
		return err
	}))

	require.NoError(t, projection.Rebuild(ctx, appPool(t), accountID, integrationID))

	var got uuid.UUID
	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT strategy_id FROM position_strategies
			 WHERE integration_id = $1 AND instrument_id = $2`,
			integrationID, instrumentID).Scan(&got)
	}))
	require.Equal(t, strategyID, got, "the rebuild erased a tag the user set by hand")
}

// Review finding 5: the canonical order is compared in two places -- Go's byte-wise `>`
// inside the engine's cursor, and the database's collation in the ORDER BY, the keyset
// comparison and the index. Those two agree only while the column collates like C.
//
// The database is declared en_US.utf8. On the musl-based image this behaves like C, so
// today they happen to agree; on a glibc Postgres -- the standard Debian image, or any
// managed instance -- en_US.UTF-8 ignores punctuation at the primary level, and
// venue_event_id is full of colons. The orders would diverge, the fold would be handed
// two tied-timestamp events in an order Apply rejects, and the projection would stop.
// Declaring the collation removes the dependency on where this is deployed.
func TestOrderingColumnsCollateLikeGo(t *testing.T) {
	ctx := context.Background()
	pool := ownerPool(t)

	for _, c := range []struct{ table, column string }{
		{"ledger_events", "venue_event_id"},
		{"positions", "last_venue_event_id"},
		{"projection_cursors", "last_venue_event_id"},
		{"position_fees", "fee_asset"},
	} {
		var collation *string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT c.collname
			   FROM pg_attribute a
			   JOIN pg_class t ON t.oid = a.attrelid
			   LEFT JOIN pg_collation c ON c.oid = a.attcollation
			  WHERE t.relname = $1 AND a.attname = $2 AND a.attnum > 0`,
			c.table, c.column).Scan(&collation))
		require.NotNil(t, collation, "%s.%s has no explicit collation", c.table, c.column)
		require.Equal(t, "C", *collation,
			"%s.%s must collate like Go's byte comparison", c.table, c.column)
	}
}

// Review finding 6: naming an integration that does not belong to this account must be an
// error. RLS makes it return zero rows, so without a check the fold reports success having
// done nothing -- and Rebuild reports success having "dropped" a projection it could not
// see. Silence is the worst possible failure (L11).
func TestProjectingAnIntegrationThatIsNotYoursIsAnError(t *testing.T) {
	ctx := context.Background()
	accountA, _ := seedIntegration(t)
	_, integrationB := seedIntegration(t)

	_, err := projection.Project(ctx, appPool(t), accountA, integrationB)
	require.ErrorIs(t, err, projection.ErrUnknownIntegration)
	require.ErrorIs(t, projection.Rebuild(ctx, appPool(t), accountA, uuid.New()),
		projection.ErrUnknownIntegration)
}

// Review finding 7: the projection tables are droppable by definition, so they must not be
// what stops an integration -- or, through its cascade, an account -- from being deleted.
// 00002_accounts_delete_policy.sql exists precisely to keep that path open for operators.
func TestProjectionTablesCascadeRatherThanBlockDeletion(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedInstrument(t)

	appendEvents(t, accountID,
		storable(withFee(trade(ledger.SideBuy, "1", "100", 1), "0.1", "USDT"),
			accountID, integrationID, instrumentID))
	project(t, accountID, integrationID)
	require.Len(t, snapshot(t, accountID, integrationID), 1)

	for _, table := range []string{"positions", "projection_cursors", "position_strategies"} {
		var action string
		require.NoError(t, ownerPool(t).QueryRow(ctx,
			`SELECT confdeltype FROM pg_constraint
			  WHERE conrelid = $1::regclass AND contype = 'f'
			    AND confrelid = 'integrations'::regclass`, table).Scan(&action))
		require.Equal(t, "c", action,
			"%s must cascade when its integration is deleted, not block it", table)
	}

	// position_fees hangs off positions, so dropping the projection must take it too.
	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM positions WHERE integration_id = $1`, integrationID)
		return err
	}))
	var remaining int
	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM position_fees WHERE integration_id = $1`,
			integrationID).Scan(&remaining)
	}))
	require.Zero(t, remaining, "position_fees must cascade from positions")
}

// Issue #4, and M2's exact topology: the WS stream ingests live trades and the projector
// advances its cursor to today, then the REST backfill appends last year's history. Every
// backfilled row sorts below the cursor, Stream never returns it, and the position stays
// permanently wrong with nothing in freshness to say so.
//
// The order-independence test does not catch this because it appends everything before
// projecting once. This one interleaves them the way the two ingest paths will.
func TestAnEventArrivingBelowTheCursorForcesARebuild(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedInstrument(t)

	// The live stream gets there first.
	appendEvents(t, accountID,
		storable(trade(ledger.SideBuy, "1", "200", 10), accountID, integrationID, instrumentID))

	res, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)
	require.False(t, res.Rebuilt, "a first projection has nothing to rebuild")
	require.Equal(t, "200", snapshot(t, accountID, integrationID)[0].AvgEntryPrice.String())

	// The backfill then delivers an older trade, which sorts below the cursor.
	appendEvents(t, accountID,
		storable(trade(ledger.SideBuy, "1", "100", 1), accountID, integrationID, instrumentID))

	res, err = projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)
	require.True(t, res.Rebuilt, "an event behind the cursor must force a rebuild")

	rows := snapshot(t, accountID, integrationID)
	require.Len(t, rows, 1)
	require.Equal(t, "2", rows[0].Quantity.String())
	require.Equal(t, "150", rows[0].AvgEntryPrice.String(),
		"the backfilled trade must reach the position, not be skipped forever")
}

// The detector must not fire on the ordinary path, or every run becomes a full rebuild and
// the incremental fold is decorative.
func TestAnOrdinaryIncrementalRunDoesNotRebuild(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedInstrument(t)

	appendEvents(t, accountID,
		storable(trade(ledger.SideBuy, "1", "100", 1), accountID, integrationID, instrumentID))
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	appendEvents(t, accountID,
		storable(trade(ledger.SideBuy, "1", "200", 2), accountID, integrationID, instrumentID))
	res, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)
	require.False(t, res.Rebuilt, "an event after the cursor is folded forward, not rebuilt")
	require.Equal(t, 1, res.EventsFolded)
	require.Equal(t, "150", snapshot(t, accountID, integrationID)[0].AvgEntryPrice.String())
}
