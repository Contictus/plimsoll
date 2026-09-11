//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"os"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/Contictus/plimsoll/backend/internal/events"
	"github.com/Contictus/plimsoll/backend/internal/httpapi"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// seedPerp gives the account one captured perpetual position with a tier table, through the
// same Save the worker uses -- so the test exercises the storage the endpoint actually reads
// rather than a shape invented for it.
func seedCapturedPerp(
	t *testing.T, accountID, integrationID uuid.UUID,
	instrumentID int64, symbol, qty, entry, mark string,
) {
	t.Helper()
	ctx := context.Background()
	// The venue's own arithmetic: margin balance is the wallet plus the open PnL. Seeding it
	// consistently is what lets a test tell the two apart -- with a flat position they are the
	// same number, and a projection that double-counted the PnL would pass.
	pnl := d(mark).Sub(d(entry)).Mul(d(qty))
	snapshot := collateral.Snapshot{
		AsOf:              time.Now().UTC(),
		WalletBalance:     d("10000"),
		MarginBalance:     d("10000").Add(pnl),
		UnrealizedPnL:     pnl,
		MaintenanceMargin: d("500"),
		AvailableBalance:  d("9500"),
		Positions: []collateral.PositionRisk{{
			Symbol:       symbol,
			InstrumentID: instrumentID,
			Quantity:     d(qty),
			EntryPrice:   d(entry),
			MarkPrice:    d(mark),
			Notional:     d(qty).Mul(d(mark)),
			Leverage:     d("5"),
			MaintMargin:  d("500"),
		}},
		Brackets: map[string][]collateral.Bracket{symbol: {{
			Bracket:          1,
			NotionalFloor:    decimal.Zero,
			NotionalCap:      d("100000000"),
			MaintMarginRatio: d("0.01"),
			Cum:              decimal.Zero,
		}}},
	}
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		return collateral.Save(ctx, q, accountID, integrationID, snapshot, time.Now().UTC())
	}))
}

func postScenario(
	t *testing.T, srv *httptest.Server, cookie *http.Cookie, body string,
) map[string]any {
	t.Helper()
	resp := send(t, http.MethodPost, srv.URL+"/risk/scenario", body, cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return decodeJSON(t, resp)
}

// seedPerpInstrument returns the contract and the asset it is ON, because a shock names the
// base asset and a spot holding of that asset must move with it (K56).
func seedPerpInstrument(t *testing.T) (instrumentID int64, symbol, baseSymbol string) {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)

	baseSymbol = "SB" + strings.ToUpper(uuid.NewString()[:8])
	symbol = "SP" + strings.ToUpper(uuid.NewString()[:8])

	var baseID, quoteID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		baseSymbol).Scan(&baseID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'stablecoin') RETURNING id`,
		"SQ"+strings.ToUpper(uuid.NewString()[:8])).Scan(&quoteID))
	// A perp names its settle asset, which the schema requires (migration 00005).
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO instruments
		   (canonical_symbol, kind, base_asset_id, quote_asset_id, settle_asset_id)
		 VALUES ($1, 'perp', $2, $3, $3) RETURNING id`,
		symbol, baseID, quoteID).Scan(&instrumentID))
	return instrumentID, symbol, baseSymbol
}

// A long perpetual loses under a fall, and the endpoint says by how much, from the same
// captured snapshot /risk is built from.
func TestAScenarioProjectsALongPositionThroughAFall(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("scenario-long"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	instrumentID, symbol, base := seedPerpInstrument(t)

	seedCapturedPerp(t, accountID, integrationID, instrumentID, symbol, "1", "50000", "50000")

	body := postScenario(t, srv, cookie, `{"shocks":{"`+base+`":"-0.2"}}`)

	shocked := body["shocked"].(map[string]any)
	require.Equal(t, "-10000", shocked["unrealized_pnl"],
		"one unit at 50000 down twenty percent")
	// Margin balance 10000 - 10000 = 0; requirement is 40000 * 0.01 = 400.
	require.Equal(t, "0", shocked["margin_balance"])
	require.Equal(t, "400", shocked["maintenance_margin"])
	require.Equal(t, "-400", shocked["buffer"], "signed, and never clamped at zero")
	require.Equal(t, true, shocked["liquidated"])

	require.Equal(t, false, body["base"].(map[string]any)["liquidated"],
		"it is solvent before the shock, which is what makes the after meaningful")
}

// The shocks come back exactly as applied. A projection read without knowing which assets
// moved is a number with no question attached -- and the assets NOT listed held still, which
// is half of what the answer means (K56).
func TestTheShocksAreEchoedBackAsApplied(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("scenario-echo"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	instrumentID, symbol, base := seedPerpInstrument(t)
	seedCapturedPerp(t, accountID, integrationID, instrumentID, symbol, "1", "50000", "50000")

	body := postScenario(t, srv, cookie, `{"shocks":{"`+base+`":"-0.125"}}`)

	require.Equal(t, map[string]any{base: "-0.125"}, body["shocks"],
		"as a string, with every digit it was sent with (L1)")
}

// A move that would take the price to zero or below is refused, not clamped. 422 rather than
// 500: the request is understood and impossible, which is a different thing from broken.
func TestAnImpossibleMoveIsRefused(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("scenario-impossible"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	instrumentID, symbol, base := seedPerpInstrument(t)
	seedCapturedPerp(t, accountID, integrationID, instrumentID, symbol, "1", "50000", "50000")

	resp := send(t, http.MethodPost, srv.URL+"/risk/scenario",
		`{"shocks":{"`+base+`":"-1"}}`, cookie)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

// A shock that is not a number is refused too, and the asset is named so the caller knows
// which one to fix.
func TestANonNumericShockIsRefused(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("scenario-nan"))

	resp := send(t, http.MethodPost, srv.URL+"/risk/scenario",
		`{"shocks":{"BTC":"twenty percent"}}`, cookie)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

// The projection needs a session like every other view of an account.
func TestAScenarioRequiresASession(t *testing.T) {
	srv := newServer(t)
	resp := send(t, http.MethodPost, srv.URL+"/risk/scenario", `{"shocks":{"BTC":"-0.1"}}`, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// An account with nothing in it projects to nothing, rather than failing. A user who has just
// signed up is allowed to ask the question.
func TestAnEmptyAccountProjectsToZeroRatherThanFailing(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("scenario-empty"))

	body := postScenario(t, srv, cookie, `{"shocks":{"BTC":"-0.3"}}`)
	require.Equal(t, "0", body["shocked"].(map[string]any)["equity"])
	require.Equal(t, false, body["shocked"].(map[string]any)["liquidated"])
}

// A projection built on a stale capture is a projection of a stale account, and the response
// says so. A shock applied to last hour's positions is a confident answer to a question about
// an account that has since changed (L11).
func TestAProjectionOnAStaleCaptureIsDegraded(t *testing.T) {
	srv := newServerWithCollateralTTL(t, time.Nanosecond)
	cookie := register(t, srv, uniqueEmail("scenario-stale"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	instrumentID, symbol, base := seedPerpInstrument(t)
	seedCapturedPerp(t, accountID, integrationID, instrumentID, symbol, "1", "50000", "50000")

	body := postScenario(t, srv, cookie, `{"shocks":{"`+base+`":"-0.1"}}`)
	require.Contains(t, codesIn(body), "collateral_stale")
}

// An integration with no capture at all is collateral_unavailable, not a projection from a
// wallet balance of zero. An unknown margin picture and an empty one are opposite claims, and
// only one of them is safe to act on (K50).
func TestAProjectionWithNoCaptureSaysSoRatherThanAssumingZero(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("scenario-nocapture"))
	accountID := accountOf(t, srv, cookie)
	seedIntegrationFor(t, accountID)

	body := postScenario(t, srv, cookie, `{"shocks":{"BTC":"-0.1"}}`)
	require.Contains(t, codesIn(body), "collateral_unavailable")
	require.Equal(t, "unreliable", body["freshness"].(map[string]any)["status"])
}

// newServerWithCollateralTTL builds a server whose captures age on a timescale a test can
// reach, so "stale" is exercised rather than waited for.
func newServerWithCollateralTTL(t *testing.T, ttl time.Duration) *httptest.Server {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	keys, err := crypto.NewEnvFileProvider()
	require.NoError(t, err)

	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		DB:            pool,
		Auth:          auth.NewService(store.New(pool), pool, 24*time.Hour),
		Now:           time.Now,
		Keys:          keys,
		Events:        events.PoolSubscriber{Pool: pool},
		CollateralTTL: ttl,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The wallet balance is the anchor, not the margin balance.
//
// The margin balance already carries today's unrealized PnL, and the projection recomputes
// that PnL at the shocked price. Adding both counts the same profit twice -- and it does so
// invisibly, because with a flat position the two numbers are identical.
func TestTodaysUnrealizedPnlIsNotCountedTwice(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("scenario-double"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	instrumentID, symbol, base := seedPerpInstrument(t)

	// One unit bought at 50000, now marked at 60000: 10000 of open profit, so the venue's
	// margin balance is 20000 against a wallet of 10000.
	seedCapturedPerp(t, accountID, integrationID, instrumentID, symbol, "1", "50000", "60000")

	body := postScenario(t, srv, cookie, `{"shocks":{"`+base+`":"0"}}`)
	basecase := body["base"].(map[string]any)

	require.Equal(t, "10000", basecase["unrealized_pnl"])
	require.Equal(t, "20000", basecase["margin_balance"],
		"wallet 10000 plus the 10000 the projection recomputed -- not 30000")
	require.Equal(t, "20000", basecase["equity"])
}
