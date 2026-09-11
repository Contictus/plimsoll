//go:build integration

package transfer_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/transfer"
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

// world is one account with two venues and one asset -- the smallest shape in which a
// cross-venue transfer is even possible.
type world struct {
	accountID      uuid.UUID
	binance, bybit uuid.UUID
	assetID        int64
	asset          string
	seqOf          map[string]int64
}

func seedWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	w := &world{
		accountID: uuid.New(),
		binance:   uuid.New(),
		bybit:     uuid.New(),
		asset:     "TX" + uuid.NewString()[:8],
		seqOf:     map[string]int64{},
	}
	owner := ownerPool(t)

	require.NoError(t, tenancy.InTxRaw(ctx, owner, w.accountID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, email) VALUES ($1, $2)`,
			w.accountID, "xfer-"+w.accountID.String()+"@example.test"); err != nil {
			return err
		}
		for _, i := range []struct {
			id       uuid.UUID
			exchange string
		}{{w.binance, "binance"}, {w.bybit, "bybit"}} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO integrations (id, account_id, exchange, label)
				 VALUES ($1, $2, $3, 'xfer-test')`, i.id, w.accountID, i.exchange); err != nil {
				return err
			}
		}
		return nil
	}))

	require.NoError(t, owner.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		w.asset).Scan(&w.assetID))
	return w
}

// leg appends one balance event and remembers the seq it landed on, which is how a link names
// it.
func (w *world) leg(
	t *testing.T, name string, integrationID uuid.UUID, kind, amount string,
	at time.Time, txid string,
) {
	t.Helper()
	ctx := context.Background()
	raw := `{}`
	if txid != "" {
		raw = fmt.Sprintf(`{"txId":%q}`, txid)
	}
	var seq int64
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), w.accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO ledger_events
			   (account_id, integration_id, venue_event_id, venue_sequence, source,
			    event_type, asset_id, quantity, event_time, raw)
			 VALUES ($1, $2, $3, 0, 'rest', $4, $5, $6, $7, $8)
			 RETURNING seq`,
			w.accountID, integrationID, name+":"+uuid.NewString(), kind,
			w.assetID, decimal.RequireFromString(amount), at, raw).Scan(&seq)
	}))
	w.seqOf[name] = seq
}

func (w *world) reconcile(t *testing.T, at time.Time) transfer.Result {
	t.Helper()
	got, err := transfer.Reconcile(
		context.Background(), appPool(t), w.accountID, transfer.DefaultRules, at)
	require.NoError(t, err)
	return got
}

func (w *world) links(t *testing.T) []store.ListTransferLinksRow {
	t.Helper()
	ctx := context.Background()
	var rows []store.ListTransferLinksRow
	require.NoError(t, tenancy.InTx(ctx, appPool(t), w.accountID, func(q *store.Queries) error {
		var err error
		rows, err = q.ListTransferLinks(ctx, w.accountID)
		return err
	}))
	return rows
}

func (w *world) openFindings(t *testing.T) []quality.Stored {
	t.Helper()
	ctx := context.Background()
	var rows []quality.Stored
	require.NoError(t, tenancy.InTx(ctx, appPool(t), w.accountID, func(q *store.Queries) error {
		var err error
		rows, err = quality.Open(ctx, q, w.accountID)
		return err
	}))
	return rows
}

var noonUTC = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

// THE M8 EXIT TEST.
//
// Four assertions at once, and the fourth is the one a matcher that did nothing would fail:
// the link exists, the ledger is untouched, the balances are unchanged, and the two legs are
// the ones we meant. A milestone whose success is mostly the absence of change needs all four,
// which is the lesson K49 learned the same way.
func TestACrossVenueTransferIsJoinedAndChangesNoNumber(t *testing.T) {
	w := seedWorld(t)
	w.leg(t, "out", w.binance, "WITHDRAWAL", "1.0", noonUTC, "")
	w.leg(t, "in", w.bybit, "DEPOSIT", "0.999", noonUTC.Add(30*time.Minute), "")

	before := ledgerFingerprint(t, w)

	got := w.reconcile(t, noonUTC.Add(time.Hour))
	require.Len(t, got.Links, 1)

	links := w.links(t)
	require.Len(t, links, 1)
	require.Equal(t, w.seqOf["out"], links[0].OutSeq)
	require.Equal(t, w.seqOf["in"], links[0].InSeq)
	require.Equal(t, "binance", links[0].OutExchange)
	require.Equal(t, "bybit", links[0].InExchange)
	require.Equal(t, transfer.MethodHeuristic, links[0].Method)

	require.Equal(t, before, ledgerFingerprint(t, w),
		"a link is an assertion about two events; it must not have written a third")
}

// ledgerFingerprint is the whole observable state of the account's ledger. If matching touched
// a row, added one or removed one, this changes.
func ledgerFingerprint(t *testing.T, w *world) string {
	t.Helper()
	ctx := context.Background()
	var fingerprint *string
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), w.accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT md5(string_agg(
			          seq || ':' || event_type || ':' || coalesce(quantity::text, ''),
			          ',' ORDER BY seq))
			   FROM ledger_events WHERE account_id = $1`, w.accountID).Scan(&fingerprint)
	}))
	if fingerprint == nil {
		return "empty"
	}
	return *fingerprint
}

// Running the matcher twice produces the same links, not a duplicate-key error. A job that
// cannot be re-run is a job that cannot be recovered after a crash.
func TestMatchingTwiceIsTheSameAsMatchingOnce(t *testing.T) {
	w := seedWorld(t)
	w.leg(t, "out", w.binance, "WITHDRAWAL", "1.0", noonUTC, "")
	w.leg(t, "in", w.bybit, "DEPOSIT", "0.999", noonUTC.Add(30*time.Minute), "")

	w.reconcile(t, noonUTC.Add(time.Hour))
	second := w.reconcile(t, noonUTC.Add(2*time.Hour))

	require.Empty(t, second.Links, "the leg is already linked, so it is not a candidate again")
	require.Len(t, w.links(t), 1)
}

// A txid carried in the venue's own payload is read out of `raw` and decides the match. The
// two venues spell the key differently, which is why it is read by exact key rather than by a
// struct tag (F20, L15).
func TestATxidInTheRawPayloadIsUsedAsProof(t *testing.T) {
	w := seedWorld(t)
	w.leg(t, "out", w.binance, "WITHDRAWAL", "1.0", noonUTC, "0xproof")
	// A decoy that fits the heuristic better but is a different chain transaction.
	w.leg(t, "decoy", w.bybit, "DEPOSIT", "1.0", noonUTC.Add(time.Minute), "0xother")
	w.leg(t, "in", w.bybit, "DEPOSIT", "0.99", noonUTC.Add(4*time.Hour), "0xproof")

	w.reconcile(t, noonUTC.Add(5*time.Hour))

	links := w.links(t)
	require.Len(t, links, 1)
	require.Equal(t, w.seqOf["in"], links[0].InSeq, "proof beats the closer guess")
	require.Equal(t, transfer.MethodTxID, links[0].Method)
}

// A leg with no other half becomes a data-quality finding, with the consequence spelled out.
//
// This is the Binance-to-Bybit case: the outbound half was never ingestible, because Binance
// does not publish the enums that would let a withdrawal be normalized (F5, B2). Left silent,
// that documentation gap looks like a balance (K57).
func TestALonelyDepositBecomesAFindingThatNamesTheConsequence(t *testing.T) {
	w := seedWorld(t)
	w.leg(t, "orphan", w.bybit, "DEPOSIT", "1.0", noonUTC, "")

	w.reconcile(t, noonUTC.Add(time.Hour))

	findings := w.openFindings(t)
	require.Len(t, findings, 1)
	require.Equal(t, quality.KindUnmatchedTransfer, findings[0].Kind)
	require.Contains(t, findings[0].Detail, "cost basis it never had",
		"the finding says what the unmatched leg is mistaken for, not merely that it is unmatched")
}

// ...and the finding closes itself once the other half finally arrives. A register that only
// grew would make every resolved problem permanent (K53).
func TestTheFindingClosesWhenTheOtherHalfArrives(t *testing.T) {
	w := seedWorld(t)
	w.leg(t, "in", w.bybit, "DEPOSIT", "0.999", noonUTC.Add(30*time.Minute), "")
	w.reconcile(t, noonUTC.Add(time.Hour))
	require.Len(t, w.openFindings(t), 1)

	w.leg(t, "out", w.binance, "WITHDRAWAL", "1.0", noonUTC, "")
	w.reconcile(t, noonUTC.Add(2*time.Hour))

	require.Empty(t, w.openFindings(t), "the leg is linked, so it is no longer unmatched")
	require.Len(t, w.links(t), 1)
}

// The schema refuses a second claim on a leg, independently of the matcher. The manual
// endpoint writes rows the matcher never saw, so the constraint has to hold on its own.
func TestTheSchemaRefusesASecondClaimOnALeg(t *testing.T) {
	w := seedWorld(t)
	w.leg(t, "out", w.binance, "WITHDRAWAL", "1.0", noonUTC, "")
	w.leg(t, "in", w.bybit, "DEPOSIT", "0.999", noonUTC.Add(30*time.Minute), "")
	// Outside the window, so it is not a candidate and the matcher links out to in without
	// hesitating -- the ambiguity rule would otherwise leave `out` unclaimed and this test
	// would pass for the wrong reason.
	w.leg(t, "other", w.bybit, "DEPOSIT", "0.999", noonUTC.Add(7*time.Hour), "")
	w.reconcile(t, noonUTC.Add(time.Hour))
	require.Len(t, w.links(t), 1, "the withdrawal is now half of a transfer")

	ctx := context.Background()
	err := tenancy.InTx(ctx, appPool(t), w.accountID, func(q *store.Queries) error {
		return q.InsertTransferLink(ctx, store.InsertTransferLinkParams{
			AccountID: w.accountID,
			OutSeq:    w.seqOf["out"],
			InSeq:     w.seqOf["other"],
			Method:    "manual",
		})
	})
	require.Error(t, err, "that withdrawal is already half of a transfer")
}
