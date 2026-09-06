//go:build integration

package portfolio_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

var readAt = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

const loadLeaseTTL = 2 * time.Minute

func amount(s string) decimal.NullDecimal {
	return decimal.NewNullDecimal(decimal.RequireFromString(s))
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

func seedAccount(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	accountID := uuid.New()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO accounts (id, email) VALUES ($1, $2)`,
			accountID, "portfolio-"+accountID.String()+"@example.test")
		return err
	}))
	return accountID
}

func seedIntegrationFor(t *testing.T, accountID uuid.UUID, label string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', $3)`, id, accountID, label)
		return err
	}))
	return id
}

// seedPair creates one tradeable instrument with a named quote asset, because the whole
// point of the subtotals is that the quote asset is not incidental.
func seedPair(t *testing.T, quote string) (instrumentID int64, quoteSymbol string) {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)

	base := "FB-" + uuid.NewString()
	quoteSymbol = quote + "-" + uuid.NewString()

	var baseID, quoteID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		base).Scan(&baseID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'stablecoin') RETURNING id`,
		quoteSymbol).Scan(&quoteID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id)
		 VALUES ($1, 'spot', $2, $3) RETURNING id`,
		"F-"+uuid.NewString(), baseID, quoteID).Scan(&instrumentID))
	return instrumentID, quoteSymbol
}

func appendTrade(
	t *testing.T, accountID, integrationID uuid.UUID, instrumentID, seq int64,
	side ledger.Side, quantity, price, fee, feeAsset string,
) {
	t.Helper()
	ctx := context.Background()
	e := ledger.Event{
		AccountID:     accountID,
		IntegrationID: integrationID,
		VenueEventID:  fmt.Sprintf("spot:trade:%d:%d", instrumentID, seq),
		VenueSequence: seq,
		Source:        "rest",
		EventType:     ledger.TypeTrade,
		InstrumentID:  &instrumentID,
		Side:          side,
		Quantity:      amount(quantity),
		Price:         amount(price),
		EventTime:     readAt.Add(-time.Hour).Add(time.Duration(seq) * time.Second),
		Raw:           json.RawMessage(`{}`),
	}
	if fee != "" {
		e.Fee = amount(fee)
		e.FeeAsset = feeAsset
	}
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := ledger.Append(ctx, q, []ledger.Event{e})
		return err
	}))
}

func reasonCodes(r freshness.Report) []string {
	out := make([]string, 0, len(r.Reasons))
	for _, x := range r.Reasons {
		out = append(out, x.Code)
	}
	return out
}

// The whole read path: events appended, folded, and read back as a portfolio whose numbers
// are the fold's and whose subtotals are per quote asset.
func TestLoadReadsTheFoldBackAsAPortfolio(t *testing.T) {
	ctx := context.Background()
	accountID := seedAccount(t)
	integrationID := seedIntegrationFor(t, accountID, "main")
	pool := appPool(t)

	usdtPair, usdtQuote := seedPair(t, "USDT")
	btcPair, btcQuote := seedPair(t, "BTC")

	appendTrade(t, accountID, integrationID, usdtPair, 1, ledger.SideBuy, "2", "100", "0.5", "BNB")
	appendTrade(t, accountID, integrationID, usdtPair, 2, ledger.SideBuy, "2", "200", "0.5", "BNB")
	appendTrade(t, accountID, integrationID, btcPair, 3, ledger.SideBuy, "10", "0.05", "1", usdtQuote)

	_, err := projection.Project(ctx, pool, accountID, integrationID)
	require.NoError(t, err)

	got, err := portfolio.Load(ctx, pool, accountID, readAt, loadLeaseTTL)
	require.NoError(t, err)

	require.Len(t, got.Holdings, 2)
	byQuote := map[string]decimal.Decimal{}
	for _, q := range got.ByQuote {
		byQuote[q.Asset] = q.CostBasis
	}
	// 2 @ 100 then 2 @ 200 averages to 150, so four units cost 600.
	require.Equal(t, "600", byQuote[usdtQuote].String())
	require.Equal(t, "0.5", byQuote[btcQuote].String())

	fees := map[string]string{}
	for _, f := range got.Fees {
		fees[f.Asset] = f.Amount.String()
	}
	require.Equal(t, "1", fees["BNB"], "two half-BNB fees on one instrument are one BNB")
	require.Equal(t, "1", fees[usdtQuote],
		"a fee paid in an asset that is also a quote stays a fee, not a subtotal")

	require.Equal(t, readAt, got.AsOf)
}

// Events the fold has not reached mean the positions in this response are behind the events
// that produced them. Bounded by the projector's tick and self-closing -- and never silent
// (K38, L11).
func TestAnUnfoldedEventMakesTheResponseSayTheProjectionIsBehind(t *testing.T) {
	ctx := context.Background()
	accountID := seedAccount(t)
	integrationID := seedIntegrationFor(t, accountID, "main")
	pool := appPool(t)
	instrumentID, _ := seedPair(t, "USDT")

	appendTrade(t, accountID, integrationID, instrumentID, 1, ledger.SideBuy, "1", "100", "", "")
	_, err := projection.Project(ctx, pool, accountID, integrationID)
	require.NoError(t, err)

	caughtUp, err := portfolio.Load(ctx, pool, accountID, readAt, loadLeaseTTL)
	require.NoError(t, err)
	require.NotContains(t, reasonCodes(caughtUp.Freshness), freshness.ReasonProjectionLagging)

	// One more event, deliberately not folded.
	appendTrade(t, accountID, integrationID, instrumentID, 2, ledger.SideBuy, "1", "120", "", "")

	behind, err := portfolio.Load(ctx, pool, accountID, readAt, loadLeaseTTL)
	require.NoError(t, err)
	require.Contains(t, reasonCodes(behind.Freshness), freshness.ReasonProjectionLagging)
	require.Equal(t, "1", behind.Holdings[0].Quantity.String(),
		"the response must show what was folded, not what was hoped for")
}

// An account with an integration and no ledger at all is not lagging: nothing to fold is
// not the same as something unfolded, and reporting it would make the reason meaningless on
// every new connection.
func TestAnIntegrationWithNoEventsIsNotReportedAsLagging(t *testing.T) {
	ctx := context.Background()
	accountID := seedAccount(t)
	seedIntegrationFor(t, accountID, "fresh")

	got, err := portfolio.Load(ctx, appPool(t), accountID, readAt, loadLeaseTTL)
	require.NoError(t, err)

	require.NotContains(t, reasonCodes(got.Freshness), freshness.ReasonProjectionLagging)
	require.Contains(t, reasonCodes(got.Freshness), freshness.ReasonIngestStalled,
		"an integration nobody is reading is still the loudest thing this account has to say")
}

// L12, at the read layer: one account's portfolio never contains another's position, and
// the query carries account_id whether or not RLS is underneath it.
func TestOneAccountsPortfolioNeverContainsAnothers(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)

	mine := seedAccount(t)
	mineIntegration := seedIntegrationFor(t, mine, "mine")
	theirs := seedAccount(t)
	theirsIntegration := seedIntegrationFor(t, theirs, "theirs")

	instrumentID, _ := seedPair(t, "USDT")
	appendTrade(t, mine, mineIntegration, instrumentID, 1, ledger.SideBuy, "1", "100", "", "")
	appendTrade(t, theirs, theirsIntegration, instrumentID, 1, ledger.SideBuy, "999", "100", "", "")
	_, err := projection.Project(ctx, pool, mine, mineIntegration)
	require.NoError(t, err)
	_, err = projection.Project(ctx, pool, theirs, theirsIntegration)
	require.NoError(t, err)

	got, err := portfolio.Load(ctx, pool, mine, readAt, loadLeaseTTL)
	require.NoError(t, err)

	require.Len(t, got.Holdings, 1)
	require.Equal(t, "1", got.Holdings[0].Quantity.String())
	require.Equal(t, mineIntegration, got.Holdings[0].IntegrationID)
}
