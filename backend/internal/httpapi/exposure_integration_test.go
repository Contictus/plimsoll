//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func exposureOf(t *testing.T, body map[string]any, strategy string) map[string]any {
	t.Helper()
	for _, s := range body["by_strategy"].([]any) {
		row := s.(map[string]any)
		if row["strategy"] == strategy {
			return row["metrics"].(map[string]any)
		}
	}
	t.Fatalf("no strategy %q in the exposure response: %v", strategy, body["by_strategy"])
	return nil
}

func netDeltaOf(t *testing.T, book map[string]any, asset string) string {
	t.Helper()
	for _, d := range book["net_delta"].([]any) {
		row := d.(map[string]any)
		if row["asset"] == asset {
			return row["exposure"].(string)
		}
	}
	t.Fatalf("no net delta for %q", asset)
	return ""
}

// tagPosition puts a position into a group through the API the user has.
func tagPosition(
	t *testing.T, srv *httptest.Server, cookie *http.Cookie,
	integrationID uuid.UUID, instrumentID int64, strategyID string,
) {
	t.Helper()
	resp := send(t, http.MethodPut,
		fmt.Sprintf("%s/positions/%s.%d/strategy", srv.URL, integrationID, instrumentID),
		fmt.Sprintf(`{"strategy_id":%q}`, strategyID), cookie)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

// L10, and the only place it is observable: two endpoints answering from one run.
//
// The engine's own tests cannot catch this -- they are handed the numbers. What can go wrong
// here is the API valuing the same account twice, and "every screen shows a different total"
// is the failure the one-run law exists to design out.
func TestExposureAndPortfolioAgreeOnTheSameRun(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("exposure-agrees"))
	accountID := accountOf(t, srv, cookie)
	integrationID, instrumentID, _, _ := seedFoldedPosition(t, accountID)

	quoteAssetID := quoteAssetOf(t, instrumentID)
	appendDeposit(t, accountID, integrationID, quoteAssetID, "1000")
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	at := time.Now().UTC()
	seedPrice(t, instrumentID, "200", at.Add(-time.Minute))
	produceRun(t, quoteAssetID, at)

	portfolio := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))
	exposure := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/exposure", cookie))
	requireMoneyIsString(t, exposure, "exposure")

	require.Equal(t, portfolio["total_value_usd"], exposure["equity"],
		"the two endpoints valued the same account differently")
	require.Equal(t, portfolio["as_of"], exposure["as_of"])

	// 4 of the base at 200: the position is the exposure, and the quote left over is not.
	book := exposure["portfolio"].(map[string]any)
	require.Equal(t, "800", book["gross_exposure"])
	require.Equal(t, "800", book["net_exposure"])
}

// THE BASIS TRADE, through HTTP. Same claim as the engine's test, made where a client sees it.
func TestExposureReportsAHedgedPairAsFlatWithinItsStrategy(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("exposure-basis"))
	accountID := accountOf(t, srv, cookie)
	integrationID, spotID, _, _ := seedFoldedPosition(t, accountID)

	// The perp leg: the same base asset, sold short, in the same integration.
	perpID := seedPerpOn(t, baseAssetOf(t, spotID), quoteAssetOf(t, spotID))
	appendFill(t, accountID, integrationID, perpID, "sell", "4", "200", 9)
	quoteAssetID := quoteAssetOf(t, spotID)
	appendDeposit(t, accountID, integrationID, quoteAssetID, "1000")
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	at := time.Now().UTC()
	seedPrice(t, spotID, "200", at.Add(-time.Minute))
	seedPrice(t, perpID, "200", at.Add(-time.Minute))
	produceRun(t, quoteAssetID, at)

	carry := createStrategy(t, srv, cookie, "cash and carry", "basis")
	tagPosition(t, srv, cookie, integrationID, spotID, carry)
	tagPosition(t, srv, cookie, integrationID, perpID, carry)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/exposure", cookie))
	requireMoneyIsString(t, body, "exposure")

	require.Equal(t, "1600", body["portfolio"].(map[string]any)["gross_exposure"],
		"gross is both legs, and this is the number that reads as leverage")

	book := exposureOf(t, body, "cash and carry")
	require.Equal(t, "0", netDeltaOf(t, book, assetSymbolOf(t, baseAssetOf(t, spotID))),
		"the hedge is invisible: the two legs did not cancel inside their own strategy")
	require.Equal(t, "0", book["net_exposure"])
	require.Equal(t, "0", book["net_leverage"])
}

// L11: an account no run can price has NO exposure total, and the response names what it
// could not price rather than reporting an account with no risk.
//
// The assertion is on the named assets rather than on valuation_unavailable, deliberately:
// this database has runs in it from other accounts' tests, so the honest reason here is that
// these assets are not in one -- and a test that demanded the other code would be asserting
// the state of the fixture, not the behaviour.
func TestExposureWithoutPricesReportsNoTotalsAndNamesWhat(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("exposure-unpriced"))
	accountID := accountOf(t, srv, cookie)
	_, _, _, quote := seedFoldedPosition(t, accountID)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/exposure", cookie))
	require.Equal(t, "", body["equity"], "an unpriceable account is not an account worth zero")
	require.Equal(t, "", body["portfolio"].(map[string]any)["gross_exposure"],
		"an exposure of 0 and an exposure nobody could compute are opposite claims")
	require.NotEmpty(t, body["unpriced_assets"], "the response says nothing about what it is missing")
	require.NotEqual(t, "ok", body["freshness"].(map[string]any)["status"])
	require.NotEmpty(t, quote)
}

// seedPerpOn creates a perpetual on assets that already exist, so a hedge can be built out of
// the same base asset the spot leg holds. Two instruments, one asset -- which is the whole
// shape of a basis trade, and the reason an exposure engine keyed on symbols rather than
// assets could never see it (K10).
func seedPerpOn(t *testing.T, baseAssetID, quoteAssetID int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id,
		                          settle_asset_id)
		 VALUES ($1, 'perp', $2, $3, $3) RETURNING id`,
		"XP-"+uuid.NewString(), baseAssetID, quoteAssetID).Scan(&id))
	return id
}

func assetSymbolOf(t *testing.T, assetID int64) string {
	t.Helper()
	var symbol string
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`SELECT canonical_symbol FROM assets WHERE id = $1`, assetID).Scan(&symbol))
	return symbol
}

// appendFill puts one fill in the ledger and does not fold it: the caller folds once, after
// everything it seeded, the way a worker would.
func appendFill(
	t *testing.T, accountID, integrationID uuid.UUID, instrumentID int64,
	side, quantity, price string, seq int64,
) {
	t.Helper()
	ctx := context.Background()
	e := ledger.Event{
		AccountID:     accountID,
		IntegrationID: integrationID,
		VenueEventID:  fmt.Sprintf("usdm:trade:%d:%d", instrumentID, seq),
		VenueSequence: seq,
		Source:        "rest",
		EventType:     ledger.TypeTrade,
		InstrumentID:  &instrumentID,
		Side:          ledger.Side(side),
		Quantity:      decimal.NewNullDecimal(decimal.RequireFromString(quantity)),
		Price:         decimal.NewNullDecimal(decimal.RequireFromString(price)),
		EventTime:     time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second),
		Raw:           json.RawMessage(`{}`),
	}
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := ledger.Append(ctx, q, []ledger.Event{e})
		return err
	}))
}

// captureWithPnL stores a margin snapshot carrying open perpetual PnL, which is the half of
// equity the balances do not hold: a perp's profit is in no wallet until it is realized.
func captureWithPnL(
	t *testing.T, accountID, integrationID uuid.UUID, instrumentID int64, symbol string,
	asOf time.Time, unrealized string,
) {
	t.Helper()
	ctx := context.Background()
	d := decimal.RequireFromString
	s := collateral.Snapshot{
		AsOf:              asOf,
		MarginBalance:     d("1000"),
		WalletBalance:     d("1000"),
		UnrealizedPnL:     d(unrealized),
		MaintenanceMargin: d("100"),
		AvailableBalance:  d("500"),
		Positions: []collateral.PositionRisk{{
			Symbol: symbol, InstrumentID: instrumentID,
			Quantity: d("1"), EntryPrice: d("100"), MarkPrice: d("200"),
			LiquidationPrice: d("50"), Notional: d("200"), Leverage: d("2"),
			MaintMargin: d("100"),
		}},
		Brackets: map[string][]collateral.Bracket{symbol: {{
			Bracket: 1, NotionalFloor: decimal.Zero, NotionalCap: d("50000"),
			MaintMarginRatio: d("0.01"), Cum: decimal.Zero,
		}}},
	}
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		return collateral.Save(ctx, q, accountID, integrationID, s, asOf)
	}))
}

// Equity is the valued holdings PLUS the open perpetual PnL. Leaving it out understates
// equity, which overstates leverage -- for exactly the account that is winning, and at the
// moment it is deciding whether it has room to add.
func TestEquityIncludesOpenPerpetualPnL(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("equity-pnl"))
	accountID := accountOf(t, srv, cookie)
	integrationID, instrumentID, symbol, _ := seedFoldedPosition(t, accountID)

	quoteAssetID := quoteAssetOf(t, instrumentID)
	appendDeposit(t, accountID, integrationID, quoteAssetID, "1000")
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	at := time.Now().UTC()
	seedPrice(t, instrumentID, "200", at.Add(-time.Minute))
	produceRun(t, quoteAssetID, at)

	before := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/exposure", cookie))
	require.Equal(t, "1200", before["equity"])

	captureWithPnL(t, accountID, integrationID, instrumentID, symbol, at.Add(-10*time.Second), "250")

	after := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/exposure", cookie))
	require.Equal(t, "1450", after["equity"],
		"the open perpetual PnL is missing from equity, so every leverage ratio is too high")
}

// A flat position is kept for its realized PnL and it is NOT exposure. Counting it would put
// a row of zeroes in every strategy's net delta, and a screen listing exposures that are not
// exposures is one nobody reads carefully.
func TestAClosedPositionIsNotExposure(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("flat-exposure"))
	accountID := accountOf(t, srv, cookie)
	integrationID, instrumentID, _, _ := seedFoldedPosition(t, accountID)

	quoteAssetID := quoteAssetOf(t, instrumentID)
	appendDeposit(t, accountID, integrationID, quoteAssetID, "1000")
	// Sells everything the two seeded fills bought.
	appendFill(t, accountID, integrationID, instrumentID, "sell", "4", "200", 7)
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	at := time.Now().UTC()
	seedPrice(t, instrumentID, "200", at.Add(-time.Minute))
	produceRun(t, quoteAssetID, at)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/exposure", cookie))
	book := body["portfolio"].(map[string]any)
	require.Equal(t, "0", book["gross_exposure"])
	require.Empty(t, book["net_delta"],
		"a closed position is being reported as an exposure of zero rather than as no exposure")
}
