//go:build integration

package ledger_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// These tests never tear down what they write, and that is the point: ledger_events has
// no DELETE policy and no DELETE grant, so nothing in this repository -- test code
// included -- has a path to remove an event (L2). The accounts and integrations behind
// them survive for the same reason, held in place by the composite FK. `make down` drops
// the volume when a developer wants an empty database.

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

// seedIntegration creates an account and one integration under it. Both inserts run as
// the owner with the account bound, because FORCE ROW LEVEL SECURITY binds the owner too.
func seedIntegration(t *testing.T) (accountID, integrationID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	accountID, integrationID = uuid.New(), uuid.New()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, email) VALUES ($1, $2)`,
			accountID, "ledger-"+accountID.String()+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'test')`, integrationID, accountID)
		return err
	}))
	return accountID, integrationID
}

// appendAs runs ledger.Append through the same path production uses: the app role, inside
// tenancy.InTx, with the account bound.
func appendAs(t *testing.T, accountID uuid.UUID, events ...ledger.Event) (int, error) {
	t.Helper()
	var inserted int
	err := tenancy.InTx(context.Background(), appPool(t), accountID,
		func(q *store.Queries) error {
			var err error
			inserted, err = ledger.Append(context.Background(), q, events)
			return err
		})
	return inserted, err
}

func streamAs(t *testing.T, accountID, integrationID uuid.UUID) []ledger.Event {
	t.Helper()
	var got []ledger.Event
	require.NoError(t, tenancy.InTx(context.Background(), appPool(t), accountID,
		func(q *store.Queries) error {
			var err error
			got, err = ledger.Stream(context.Background(), q,
				accountID, integrationID, ledger.Cursor{}, 100)
			return err
		}))
	return got
}

func at(offset time.Duration) time.Time {
	return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Add(offset)
}

// deposit is the minimal storable event: no instrument, no price, no side.
func deposit(accountID, integrationID uuid.UUID, venueEventID string, seq int64, when time.Time) ledger.Event {
	return ledger.Event{
		AccountID:     accountID,
		IntegrationID: integrationID,
		VenueEventID:  venueEventID,
		VenueSequence: seq,
		Source:        "rest",
		EventType:     ledger.TypeDeposit,
		Quantity:      decimal.NewNullDecimal(decimal.RequireFromString("1")),
		EventTime:     when,
		Raw:           json.RawMessage(`{"probe":"deposit"}`),
	}
}

// THE TEST THIS TASK EXISTS FOR (L5, K19).
//
// REST backfill and the WebSocket stream report the same trade under different `source`
// values. K3's original key was (integration_id, source, external_id), which would have
// inserted both rows and doubled the position -- silently, with plausible numbers. Source
// is metadata: who saw it first, never identity.
func TestTheSameEventFromTwoSourcesIsStoredOnce(t *testing.T) {
	accountID, integrationID := seedIntegration(t)

	seenFirstByREST := deposit(accountID, integrationID, "spot:deposit:1", 1, at(0))
	seenLaterByWS := seenFirstByREST
	seenLaterByWS.Source = "ws"
	seenLaterByWS.Raw = json.RawMessage(`{"probe":"deposit","via":"ws"}`)

	inserted, err := appendAs(t, accountID, seenFirstByREST)
	require.NoError(t, err)
	require.Equal(t, 1, inserted)

	// "We saw 1 event and stored 0" is the signal that makes a duplicate backfill visible
	// instead of silent, so Append reports what it actually wrote.
	inserted, err = appendAs(t, accountID, seenLaterByWS)
	require.NoError(t, err)
	require.Equal(t, 0, inserted, "the same venue event under a second source must not insert")

	got := streamAs(t, accountID, integrationID)
	require.Len(t, got, 1, "one exchange event is one ledger row, whichever path saw it")
	require.Equal(t, "rest", got[0].Source, "source records who saw it first")
}

// L7, K21: canonical order is (event_time, venue_sequence, venue_event_id). Exchange fills
// of one order share a timestamp to the millisecond, so ordering by event_time alone is
// undefined -- and an undefined fold order makes avg_entry_price depend on ingest order.
// The events below are appended scrambled so a passing result cannot be an artifact of
// insertion order.
func TestCanonicalOrderBreaksTiesBySequenceThenVenueEventID(t *testing.T) {
	accountID, integrationID := seedIntegration(t)
	tied := at(0)

	later := deposit(accountID, integrationID, "spot:deposit:later", 1, at(time.Second))
	seqThree := deposit(accountID, integrationID, "spot:deposit:c", 3, tied)
	tiedB := deposit(accountID, integrationID, "spot:deposit:b", 1, tied)
	tiedA := deposit(accountID, integrationID, "spot:deposit:a", 1, tied)

	inserted, err := appendAs(t, accountID, seqThree, later, tiedB, tiedA)
	require.NoError(t, err)
	require.Equal(t, 4, inserted)

	var order []string
	for _, e := range streamAs(t, accountID, integrationID) {
		order = append(order, e.VenueEventID)
	}
	require.Equal(t, []string{
		"spot:deposit:a", // same time, same sequence: venue_event_id is the total order
		"spot:deposit:b",
		"spot:deposit:c", // same time, higher sequence
		"spot:deposit:later",
	}, order)
}

// L15: the raw payload is the project's insurance policy. When a normalization bug
// surfaces months later, replaying from raw is what saves it -- so an event without one
// cannot be stored at all.
func TestAnEventCannotBeStoredWithoutItsRawPayload(t *testing.T) {
	accountID, integrationID := seedIntegration(t)

	naked := deposit(accountID, integrationID, "spot:deposit:naked", 1, at(0))
	naked.Raw = nil

	_, err := appendAs(t, accountID, naked)
	require.Error(t, err)
	require.Contains(t, err.Error(), "raw")

	require.Empty(t, streamAs(t, accountID, integrationID))
}

// L2: the ledger is append-only, and that is a privilege rather than a convention. The
// application role holds SELECT and INSERT and nothing else, so a correction has no choice
// but to be a new event.
func TestTheAppRoleCannotUpdateOrDeleteALedgerEvent(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)

	inserted, err := appendAs(t, accountID, deposit(accountID, integrationID, "spot:deposit:frozen", 1, at(0)))
	require.NoError(t, err)
	require.Equal(t, 1, inserted)

	pool := appPool(t)
	for _, privilege := range []string{"UPDATE", "DELETE"} {
		var granted bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT has_table_privilege(current_user, 'ledger_events', $1)`, privilege).Scan(&granted))
		require.False(t, granted, "the app role must not hold %s on ledger_events", privilege)
	}

	// The privilege check states the rule; these statements prove Postgres enforces it.
	err = tenancy.InTxRaw(ctx, pool, accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE ledger_events SET source = 'rewritten' WHERE account_id = $1`, accountID)
		return err
	})
	require.ErrorContains(t, err, "permission denied")

	err = tenancy.InTxRaw(ctx, pool, accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM ledger_events WHERE account_id = $1`, accountID)
		return err
	})
	require.ErrorContains(t, err, "permission denied")

	require.Len(t, streamAs(t, accountID, integrationID), 1, "the event is still there, unchanged")
}

// K15, ARCHITECTURE.md §2: attaching an event to another account's integration is
// impossible at the storage layer, not merely unlikely. Two defences, tested separately
// because each catches a different mistake.
func TestAnEventCannotBeAttachedToAnotherAccountsIntegration(t *testing.T) {
	accountA, _ := seedIntegration(t)
	accountB, integrationB := seedIntegration(t)

	// Right account_id (so RLS is satisfied), another account's integration. Only the
	// composite FK can catch this one -- referential checks bypass RLS by design.
	crossFK := deposit(accountA, integrationB, "spot:deposit:cross-fk", 1, at(0))
	_, err := appendAs(t, accountA, crossFK)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ledger_integration_belongs_to_account")

	// Another account's pair entirely, written from account A's session. RLS refuses the
	// row before the FK is ever consulted.
	stolen := deposit(accountB, integrationB, "spot:deposit:cross-rls", 1, at(0))
	_, err = appendAs(t, accountA, stolen)
	require.ErrorContains(t, err, "row-level security")

	require.Empty(t, streamAs(t, accountB, integrationB))
}

// The L12 backstop for this table: an unscoped read from another account returns nothing.
func TestOneAccountCannotStreamAnothersLedger(t *testing.T) {
	ctx := context.Background()
	accountA, integrationA := seedIntegration(t)
	accountB, _ := seedIntegration(t)

	_, err := appendAs(t, accountA, deposit(accountA, integrationA, "spot:deposit:private", 1, at(0)))
	require.NoError(t, err)

	require.Len(t, streamAs(t, accountA, integrationA), 1)
	require.Empty(t, streamAs(t, accountB, integrationA),
		"account B must not read account A's ledger even when it names the integration")

	// And with no account bound at all: no rows, rather than every row.
	var count int
	require.NoError(t, appPool(t).QueryRow(ctx,
		`SELECT count(*) FROM ledger_events`).Scan(&count))
	require.Zero(t, count)
}

// seedInstrument creates one tradable instrument and its two legs, as the registry tests
// do. The ledger references instruments; it does not own them.
func seedInstrument(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)

	var base, quote, id int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"LB-"+uuid.NewString()).Scan(&base))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"LQ-"+uuid.NewString()).Scan(&quote))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id,
		                          settle_asset_id)
		 VALUES ($1, 'perp', $2, $3, $3) RETURNING id`,
		"L-"+uuid.NewString(), base, quote).Scan(&id))
	return id
}

// L1: every digit of NUMERIC(38,18) survives the append/stream round trip. Verified on the
// real path rather than on a bare SELECT, because a driver mapping is only as good as the
// query that uses it.
func TestATradeKeepsEveryDigitThroughTheLedger(t *testing.T) {
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedInstrument(t)

	quantity := decimal.RequireFromString("0.000000000000000001")
	price := decimal.RequireFromString("12345678901234567890.123456789012345678")
	fee := decimal.RequireFromString("0.000750000000000001")

	trade := deposit(accountID, integrationID, "usdm:trade:BTCUSDT:99", 99, at(0))
	trade.EventType = ledger.TypeTrade
	trade.InstrumentID = &instrumentID
	trade.Side = ledger.SideBuy
	trade.Quantity = decimal.NewNullDecimal(quantity)
	trade.Price = decimal.NewNullDecimal(price)
	// L9: the fee rides on the event that caused it, and is never folded into the price.
	trade.Fee = decimal.NewNullDecimal(fee)
	trade.FeeAsset = "BNB"

	inserted, err := appendAs(t, accountID, trade)
	require.NoError(t, err)
	require.Equal(t, 1, inserted)

	got := streamAs(t, accountID, integrationID)
	require.Len(t, got, 1)
	require.Equal(t, quantity.String(), got[0].Quantity.Decimal.String())
	require.Equal(t, price.String(), got[0].Price.Decimal.String())
	require.Equal(t, fee.String(), got[0].Fee.Decimal.String())
	require.Equal(t, "BNB", got[0].FeeAsset)
	require.Equal(t, instrumentID, *got[0].InstrumentID)
	require.Equal(t, ledger.SideBuy, got[0].Side)
}

// The fold reads the ledger in pages, and a cursor that skips or repeats an event
// corrupts every projection built on it (L6: the cursor is the canonical order, never the
// global seq). Paging one event at a time is the harshest version of that test.
func TestStreamPagesForwardWithoutSkippingOrRepeating(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	tied := at(0)

	// Two events share a timestamp and a sequence, so paging has to carry all three parts
	// of the cursor to make progress at all.
	batch := []ledger.Event{
		deposit(accountID, integrationID, "spot:deposit:p1", 1, tied),
		deposit(accountID, integrationID, "spot:deposit:p2", 1, tied),
		deposit(accountID, integrationID, "spot:deposit:p3", 2, tied),
		deposit(accountID, integrationID, "spot:deposit:p4", 3, at(time.Second)),
	}
	inserted, err := appendAs(t, accountID, batch...)
	require.NoError(t, err)
	require.Equal(t, len(batch), inserted)

	var seen []string
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		cursor := ledger.Cursor{}
		for {
			page, err := ledger.Stream(ctx, q, accountID, integrationID, cursor, 1)
			if err != nil {
				return err
			}
			if len(page) == 0 {
				return nil
			}
			require.Len(t, page, 1)
			seen = append(seen, page[0].VenueEventID)
			cursor = page[0].Cursor()
		}
	}))

	require.Equal(t, []string{
		"spot:deposit:p1", "spot:deposit:p2", "spot:deposit:p3", "spot:deposit:p4",
	}, seen)
}
