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
	require.NoError(t,
		projection.Project(context.Background(), appPool(t), accountID, integrationID))
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
