//go:build integration

package reconciliation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/Contictus/plimsoll/backend/internal/reconciliation"
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

var at = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

// world is one account with one integration, one asset, and a fold that says it holds
// something. Every test below changes exactly one side of the comparison.
type world struct {
	accountID, integrationID uuid.UUID
	assetSymbol              string
	assetID                  int64
}

func seedWorld(t *testing.T, held string) world {
	t.Helper()
	ctx := context.Background()
	w := world{accountID: uuid.New(), integrationID: uuid.New()}
	w.assetSymbol = "RC" + uuid.NewString()[:8]
	owner := ownerPool(t)

	require.NoError(t, tenancy.InTxRaw(ctx, owner, w.accountID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, email) VALUES ($1, $2)`,
			w.accountID, "recon-"+w.accountID.String()+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'recon-test')`, w.integrationID, w.accountID)
		return err
	}))

	require.NoError(t, owner.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		w.assetSymbol).Scan(&w.assetID))

	// The alias the venue's own code resolves through. Reconciliation must go through this
	// rather than matching codes directly (L8, K22).
	_, err := owner.Exec(ctx,
		`INSERT INTO asset_aliases (asset_id, source, external_symbol, validity)
		 VALUES ($1, 'binance', $2, tstzrange($3, NULL, '[)'))`,
		w.assetID, w.assetSymbol, at.Add(-365*24*time.Hour))
	require.NoError(t, err)

	require.NoError(t, tenancy.InTxRaw(ctx, owner, w.accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO asset_balances
			   (account_id, integration_id, asset_id, quantity, last_event_time,
			    last_venue_sequence, last_venue_event_id)
			 VALUES ($1, $2, $3, $4, $5, 1, 'seed')`,
			w.accountID, w.integrationID, w.assetID, decimal.RequireFromString(held), at)
		return err
	}))
	return w
}

// venue is a fake exchange that answers with whatever the test wants it to believe.
type venue struct {
	balances map[string]string
	err      error
}

func (v venue) Account(context.Context) (json.RawMessage, error) {
	if v.err != nil {
		return nil, v.err
	}
	body := `{"updateTime":` + fmt.Sprint(at.UnixMilli()) + `,"balances":[`
	first := true
	for asset, free := range v.balances {
		if !first {
			body += ","
		}
		first = false
		body += `{"asset":"` + asset + `","free":"` + free + `","locked":"0"}`
	}
	return json.RawMessage(body + `]}`), nil
}

func run(t *testing.T, w world, v venue, now time.Time) error {
	t.Helper()
	return reconciliation.Run(context.Background(), reconciliation.Deps{
		DB:            appPool(t),
		AccountID:     w.accountID,
		IntegrationID: w.integrationID,
		Source:        v,
		Tolerance: reconciliation.Tolerance{
			DefaultDust: decimal.RequireFromString("0.00000001"),
		},
		SkewTolerance: time.Hour,
	}, now)
}

func openOf(t *testing.T, w world) []quality.Stored {
	t.Helper()
	ctx := context.Background()
	var out []quality.Stored
	require.NoError(t, tenancy.InTx(ctx, appPool(t), w.accountID, func(q *store.Queries) error {
		var err error
		out, err = quality.Open(ctx, q, w.accountID)
		return err
	}))
	return out
}

// The milestone in one test: the ledger and the venue disagree, and the register says so.
//
// freshness.ReasonReconciliationMismatch has existed since M0 with nothing able to produce
// it. This is the test that gives it a producer.
func TestADisagreementBecomesAnOpenFinding(t *testing.T) {
	w := seedWorld(t, "1.5")
	require.NoError(t, run(t, w, venue{balances: map[string]string{w.assetSymbol: "1.0"}}, at))

	open := openOf(t, w)
	require.Len(t, open, 1)
	require.Equal(t, quality.KindMissingEvent, open[0].Kind,
		"a gap nothing in the ledger explains is the residual")
	require.Equal(t, w.assetSymbol, open[0].Subject)
	require.Equal(t, "0.5", open[0].Delta.Decimal.String(), "ours minus theirs")
}

// A run that agrees closes what a previous run opened. Without this the register only ever
// grows, and nothing in it can be read as current.
func TestAgreementClosesWhatDisagreementOpened(t *testing.T) {
	w := seedWorld(t, "1.5")
	require.NoError(t, run(t, w, venue{balances: map[string]string{w.assetSymbol: "1.0"}}, at))
	require.Len(t, openOf(t, w), 1)

	require.NoError(t, run(t, w,
		venue{balances: map[string]string{w.assetSymbol: "1.5"}}, at.Add(5*time.Minute)))
	require.Empty(t, openOf(t, w))
}

// The payload the finding was decided from is stored with it (L15). Without it, a
// classification that turns out wrong in three months cannot be re-argued -- and K55 makes
// exactly that argument the precondition for ever correcting anything automatically.
func TestTheSnapshotIsStoredWithTheFinding(t *testing.T) {
	w := seedWorld(t, "1.5")
	require.NoError(t, run(t, w, venue{balances: map[string]string{w.assetSymbol: "1.0"}}, at))

	raw := openOf(t, w)[0].Raw
	require.NotEmpty(t, raw)
	var payload struct {
		Balances []struct {
			Asset string `json:"asset"`
			Free  string `json:"free"`
		} `json:"balances"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Equal(t, w.assetSymbol, payload.Balances[0].Asset)
	require.Equal(t, "1.0", payload.Balances[0].Free,
		"the venue's own bytes, not our summary of them")
}

// An exchange call that fails is itself a finding.
//
// "We could not check" and "we checked and it was fine" are different claims, and serving the
// second while the first is true is the exact failure L11 exists to prevent. A run that
// swallowed the error would leave the register saying nothing is wrong.
func TestAFailedSnapshotIsReportedRatherThanSkipped(t *testing.T) {
	w := seedWorld(t, "1.5")

	err := run(t, w, venue{err: errors.New("the venue returned 503")}, at)
	require.Error(t, err)

	open := openOf(t, w)
	require.Len(t, open, 1)
	require.Equal(t, quality.KindSnapshotFailed, open[0].Kind)
	require.False(t, open[0].Delta.Valid, "a failed check has no magnitude, and zero would claim one")
}

// A failed snapshot must not close the findings the last successful run opened. The venue
// being unreachable is not evidence that a disagreement went away -- and reporting that it
// did, at the moment we can least verify it, is the worst available answer.
func TestAFailedSnapshotDoesNotCloseWhatAPreviousRunFound(t *testing.T) {
	w := seedWorld(t, "1.5")
	require.NoError(t, run(t, w, venue{balances: map[string]string{w.assetSymbol: "1.0"}}, at))

	require.Error(t, run(t, w, venue{err: errors.New("the venue returned 503")}, at.Add(time.Minute)))

	kinds := map[string]bool{}
	for _, f := range openOf(t, w) {
		kinds[f.Kind] = true
	}
	require.True(t, kinds[quality.KindMissingEvent],
		"the disagreement is still open: we did not learn that it was resolved")
	require.True(t, kinds[quality.KindSnapshotFailed])
}

// An unattributed fee explains a gap of exactly its own size, so the gap is `unsupported`
// rather than a missing event. This is K54's evidence rule against a real ledger: the fee was
// stored with a venue code that resolved to nothing, so our balance is high by that much.
func TestAnUnattributedFeeExplainsTheGapItCaused(t *testing.T) {
	w := seedWorld(t, "1.5")
	ctx := context.Background()

	// The realistic shape: a fill whose fee asset code resolved to nothing, so the fee was
	// stored with its venue code and no asset id (K22). A standalone FEE event cannot carry
	// this case -- the schema requires it to name an instrument (migration 00009).
	var quoteID, instrumentID int64
	require.NoError(t, ownerPool(t).QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'stablecoin') RETURNING id`,
		"RQ-"+uuid.NewString()).Scan(&quoteID))
	require.NoError(t, ownerPool(t).QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id)
		 VALUES ($1, 'spot', $2, $3) RETURNING id`,
		"RI-"+uuid.NewString(), w.assetID, quoteID).Scan(&instrumentID))

	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), w.accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_events
			   (account_id, integration_id, venue_event_id, venue_sequence, source,
			    event_type, instrument_id, side, quantity, price, event_time,
			    fee, fee_asset, raw)
			 VALUES ($1, $2, 'trade:1', 1, 'rest', 'TRADE', $3, 'buy', 1, 1, $4, $5, $6, '{}')`,
			w.accountID, w.integrationID, instrumentID, at,
			decimal.RequireFromString("0.5"), w.assetSymbol)
		return err
	}))

	require.NoError(t, run(t, w, venue{balances: map[string]string{w.assetSymbol: "1.0"}}, at))

	open := openOf(t, w)
	require.Len(t, open, 1)
	require.Equal(t, quality.KindUnsupported, open[0].Kind,
		"a fee we could not attribute is a known gap, not a missing event")
}

// One account's register is one account's. The write goes through tenancy like every other.
func TestARunWritesOnlyIntoItsOwnAccount(t *testing.T) {
	mine := seedWorld(t, "1.5")
	theirs := seedWorld(t, "1.5")

	require.NoError(t, run(t, mine, venue{balances: map[string]string{mine.assetSymbol: "1.0"}}, at))

	require.Len(t, openOf(t, mine), 1)
	require.Empty(t, openOf(t, theirs))
}
