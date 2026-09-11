//go:build integration

package quality_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/Contictus/plimsoll/backend/internal/store"
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
			accountID, "quality-"+accountID.String()+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'quality-test')`, integrationID, accountID)
		return err
	}))
	return accountID, integrationID
}

// record runs one reconciliation pass worth of findings through the lifetime.
func record(
	t *testing.T, pool *pgxpool.Pool, accountID, integrationID uuid.UUID,
	at time.Time, found ...quality.Finding,
) {
	t.Helper()
	recordPass(t, pool, accountID, integrationID, quality.Pass{
		At: at, Owns: quality.ReconciliationKinds, Found: found,
	})
}

func recordPass(
	t *testing.T, pool *pgxpool.Pool, accountID, integrationID uuid.UUID, p quality.Pass,
) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, tenancy.InTx(ctx, pool, accountID, func(q *store.Queries) error {
		return quality.Record(ctx, q, accountID, integrationID, p)
	}))
}

func openFindings(t *testing.T, pool *pgxpool.Pool, accountID uuid.UUID) []quality.Stored {
	t.Helper()
	ctx := context.Background()
	var out []quality.Stored
	require.NoError(t, tenancy.InTx(ctx, pool, accountID, func(q *store.Queries) error {
		var err error
		out, err = quality.Open(ctx, q, accountID)
		return err
	}))
	return out
}

func allFindings(t *testing.T, pool *pgxpool.Pool, accountID uuid.UUID) []quality.Stored {
	t.Helper()
	ctx := context.Background()
	var out []quality.Stored
	require.NoError(t, tenancy.InTx(ctx, pool, accountID, func(q *store.Queries) error {
		var err error
		out, err = quality.History(ctx, q, accountID)
		return err
	}))
	return out
}

func mismatch(subject, delta string) quality.Finding {
	return quality.Finding{
		Kind:     quality.KindMissingEvent,
		Subject:  subject,
		Severity: quality.SeverityError,
		Detail:   "the exchange and the ledger disagree",
		Delta:    decimal.NewNullDecimal(decimal.RequireFromString(delta)),
		Raw:      json.RawMessage(`{"asset":"` + subject + `"}`),
	}
}

// Two runs seeing the same problem produce ONE finding, not two.
//
// Reconciliation runs every five minutes. A balance that disagrees all day is one problem the
// user has; if each run inserted a row the register would grow by 288 rows a day per subject
// and "is it still wrong?" would become a query over history instead of a row (K53).
func TestTheSameFindingSeenTwiceStaysOneFinding(t *testing.T) {
	accountID, integrationID := seedAccount(t)
	pool := appPool(t)
	first := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

	record(t, pool, accountID, integrationID, first, mismatch("BTC", "0.5"))
	record(t, pool, accountID, integrationID, first.Add(5*time.Minute), mismatch("BTC", "0.5"))

	open := openFindings(t, pool, accountID)
	require.Len(t, open, 1, "the same problem seen twice must be one finding")
	require.Equal(t, int32(2), open[0].Occurrences)
	require.True(t, open[0].OpenedAt.Equal(first),
		"opened_at must stay the instant the problem STARTED, not the last time it was seen")
	require.True(t, open[0].LastSeenAt.Equal(first.Add(5*time.Minute)))
}

// A finding the next run does not see is closed, carrying the instant it stopped being true.
// Without this the register only grows, and nothing in it can be trusted as current.
func TestAFindingThatStopsBeingTrueIsClosed(t *testing.T) {
	accountID, integrationID := seedAccount(t)
	pool := appPool(t)
	at := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

	record(t, pool, accountID, integrationID, at, mismatch("BTC", "0.5"))
	require.Len(t, openFindings(t, pool, accountID), 1)

	// The next run finds nothing wrong.
	record(t, pool, accountID, integrationID, at.Add(5*time.Minute))

	require.Empty(t, openFindings(t, pool, accountID),
		"a problem that stopped being true must not still be open")
}

// Closing is not deletion. The history of a finding is the evidence for its classification,
// and K55 makes that evidence the precondition for ever acting on one automatically.
func TestAClosedFindingIsStillReadable(t *testing.T) {
	accountID, integrationID := seedAccount(t)
	pool := appPool(t)
	at := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

	record(t, pool, accountID, integrationID, at, mismatch("BTC", "0.5"))
	record(t, pool, accountID, integrationID, at.Add(5*time.Minute))

	all := allFindings(t, pool, accountID)
	require.Len(t, all, 1)
	require.True(t, all[0].ClosedAt.Valid, "the closed finding must still be readable")
	require.True(t, all[0].ClosedAt.Time.Equal(at.Add(5*time.Minute)))
	require.JSONEq(t, `{"asset":"BTC"}`, string(all[0].Raw),
		"the payload it was decided from is kept with it (L15)")
}

// A problem that returns after closing is a NEW finding, not a resurrection.
//
// "Wrong for an hour, right for a day, wrong again" is two incidents. Collapsing them into
// one row erases the recovery in between, which is the part that tells the user whether their
// last resync worked.
func TestAFindingThatReturnsAfterClosingIsANewFinding(t *testing.T) {
	accountID, integrationID := seedAccount(t)
	pool := appPool(t)
	at := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

	record(t, pool, accountID, integrationID, at, mismatch("BTC", "0.5"))
	record(t, pool, accountID, integrationID, at.Add(1*time.Hour))
	record(t, pool, accountID, integrationID, at.Add(2*time.Hour), mismatch("BTC", "0.5"))

	open := openFindings(t, pool, accountID)
	require.Len(t, open, 1)
	require.True(t, open[0].OpenedAt.Equal(at.Add(2*time.Hour)),
		"the second incident opened when it returned, not when the first one did")
	require.Equal(t, int32(1), open[0].Occurrences,
		"a returning problem starts counting again; carrying the old count would merge two incidents")

	require.Len(t, allFindings(t, pool, accountID), 2, "two incidents, two rows")
}

// A pass closes only what it was looking for.
//
// Two different runs write into one register: the coherence checks need no exchange call, and
// reconciliation does. If either one's close sweep took the whole integration, a quiet
// reconciliation run would silently clear a negative balance nobody has fixed -- the register
// would announce that a problem went away because a different process looked elsewhere.
func TestAPassDoesNotCloseAFindingItWasNeverLookingFor(t *testing.T) {
	accountID, integrationID := seedAccount(t)
	pool := appPool(t)
	at := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

	recordPass(t, pool, accountID, integrationID, quality.Pass{
		At:   at,
		Owns: quality.CoherenceKinds,
		Found: []quality.Finding{{
			Kind:     quality.KindNegativeBalance,
			Subject:  "ETH",
			Severity: quality.SeverityError,
			Detail:   "the ledger implies selling more than was ever held",
			Delta:    decimal.NewNullDecimal(decimal.RequireFromString("-3")),
		}},
	})
	require.Len(t, openFindings(t, pool, accountID), 1)

	// A reconciliation pass that finds nothing. It owns none of the coherence kinds.
	record(t, pool, accountID, integrationID, at.Add(5*time.Minute))

	open := openFindings(t, pool, accountID)
	require.Len(t, open, 1, "a reconciliation pass must not close a coherence finding")
	require.Equal(t, quality.KindNegativeBalance, open[0].Kind)
}

// One account's register is not another's, with the application-level predicate deliberately
// absent: whatever comes back comes back from RLS alone (L12, K15).
func TestAFindingIsNotVisibleToAnotherAccount(t *testing.T) {
	mine, myIntegration := seedAccount(t)
	theirs, _ := seedAccount(t)
	pool := appPool(t)
	at := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)

	record(t, pool, mine, myIntegration, at, mismatch("BTC", "0.5"))

	var count int
	ctx := context.Background()
	require.NoError(t, tenancy.InTxRaw(ctx, pool, theirs, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM findings`).Scan(&count)
	}))
	require.Zero(t, count, "an unscoped SELECT must still return only the current account")
}
