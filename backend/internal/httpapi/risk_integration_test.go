//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// seedPerp creates a USD-M perpetual with its own assets, so two tests never share one.
func seedPerp(t *testing.T) (instrumentID int64, symbol, settle string) {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)

	symbol = "RP-" + strings.ToUpper(uuid.NewString())
	settle = "RS-" + strings.ToUpper(uuid.NewString())

	var baseID, quoteID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"RB-"+strings.ToUpper(uuid.NewString())).Scan(&baseID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'stablecoin') RETURNING id`,
		settle).Scan(&quoteID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id,
		                          settle_asset_id)
		 VALUES ($1, 'perp', $2, $3, $3) RETURNING id`,
		symbol, baseID, quoteID).Scan(&instrumentID))
	return instrumentID, symbol, settle
}

// capture stores one collateral snapshot the way the worker's tick does -- through
// collateral.Save, never by writing the tables by hand, so an endpoint tested here is
// reading what the capture actually produces.
func capture(
	t *testing.T, accountID, integrationID uuid.UUID, instrumentID int64, symbol string,
	asOf time.Time, marginBalance, maintenance, mark, liquidation, notional string,
) {
	t.Helper()
	ctx := context.Background()
	d := decimal.RequireFromString

	s := collateral.Snapshot{
		AsOf:              asOf,
		MarginBalance:     d(marginBalance),
		WalletBalance:     d(marginBalance),
		UnrealizedPnL:     decimal.Zero,
		MaintenanceMargin: d(maintenance),
		// Deliberately NOT margin less maintenance, which is the buffer: two fields that
		// happened to hold the same number would let a body render either one in the
		// other's place and still pass.
		AvailableBalance: d(marginBalance).Div(d("4")),
		Positions: []collateral.PositionRisk{{
			Symbol:           symbol,
			InstrumentID:     instrumentID,
			Quantity:         d("0.5"),
			EntryPrice:       d("60000"),
			MarkPrice:        d(mark),
			LiquidationPrice: d(liquidation),
			Notional:         d(notional),
			Leverage:         d("10"),
			MaintMargin:      d(maintenance),
		}},
		Brackets: map[string][]collateral.Bracket{symbol: {{
			Bracket:          1,
			NotionalFloor:    decimal.Zero,
			NotionalCap:      d("50000"),
			MaintMarginRatio: d("0.01"),
			Cum:              decimal.Zero,
		}}},
	}
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		return collateral.Save(ctx, q, accountID, integrationID, s, asOf)
	}))
}

// appendFunding puts one funding payment in the ledger and folds it, as the worker would.
func appendFunding(
	t *testing.T, accountID, integrationID uuid.UUID, instrumentID int64,
	amount string, seq int64, at time.Time,
) {
	t.Helper()
	ctx := context.Background()
	e := ledger.Event{
		AccountID:     accountID,
		IntegrationID: integrationID,
		VenueEventID:  fmt.Sprintf("usdm:income:FUNDING_FEE:%d", seq),
		VenueSequence: 0,
		Source:        "rest",
		EventType:     ledger.TypeFundingPayment,
		InstrumentID:  &instrumentID,
		Quantity:      decimal.NewNullDecimal(decimal.RequireFromString(amount)),
		EventTime:     at,
		Raw:           json.RawMessage(`{"incomeType":"FUNDING_FEE"}`),
	}
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := ledger.Append(ctx, q, []ledger.Event{e})
		return err
	}))
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)
}

func integrationsOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	out, ok := body["integrations"].([]any)
	require.True(t, ok, "no integrations in %v", body)
	return out
}

func reasonCodes(t *testing.T, body map[string]any) []string {
	t.Helper()
	report := body["freshness"].(map[string]any)
	out := []string{}
	for _, r := range report["reasons"].([]any) {
		out = append(out, r.(map[string]any)["code"].(string))
	}
	return out
}

// Step 1: the account can see how close it is, from ONE snapshot named in as_of (L10), and
// every number of it is a string (L1).
func TestRiskReportsEquityBufferAndLiquidationDistance(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("risk"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	instrumentID, symbol, _ := seedPerp(t)

	asOf := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Millisecond)
	capture(t, accountID, integrationID, instrumentID, symbol, asOf,
		"1000", "250", "50000", "40000", "25000")

	resp := do(t, http.MethodGet, srv.URL+"/risk", cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSON(t, resp)
	requireMoneyIsString(t, body, "risk")

	require.True(t, asOf.Equal(asOfOf(t, body)),
		"as_of must name the snapshot the numbers came from: %s, got %s",
		asOf, asOfOf(t, body))

	one := integrationsOf(t, body)[0].(map[string]any)
	require.Equal(t, "1000", one["margin_balance"])
	require.Equal(t, "250", one["maintenance_margin"])
	require.Equal(t, "750", one["margin_buffer"])
	require.Equal(t, "250", one["available_balance"],
		"available balance is the venue's own figure, not the buffer under another name")

	positions := one["positions"].([]any)
	require.Len(t, positions, 1)
	p := positions[0].(map[string]any)
	require.Equal(t, symbol, p["symbol"])
	require.Equal(t, "40000", p["liquidation_price"])
	// |50000 - 40000| / 50000
	require.Equal(t, "0.2", p["liquidation_distance"])
}

// Step 2a: a stale snapshot is served WITH its reason, never silently (L11).
func TestAStaleCaptureIsServedWithCollateralStale(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("risk-stale"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	instrumentID, symbol, _ := seedPerp(t)

	asOf := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Millisecond)
	capture(t, accountID, integrationID, instrumentID, symbol, asOf,
		"1000", "250", "50000", "40000", "25000")

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/risk", cookie))
	require.Contains(t, reasonCodes(t, body), "collateral_stale")
	require.Len(t, integrationsOf(t, body), 1,
		"a stale margin picture is still the best answer there is; it is served, not withheld")
	require.Equal(t, "750", integrationsOf(t, body)[0].(map[string]any)["margin_buffer"])
}

// Step 2b: a missing snapshot is collateral_unavailable, never a zero buffer. A zero margin
// buffer says liquidation is imminent and an unknown one says nothing at all; rendering them
// the same is the confident-and-wrong failure at the moment it costs most.
func TestAMissingCaptureIsUnavailableRatherThanAZeroBuffer(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("risk-missing"))
	accountID := accountOf(t, srv, cookie)
	seedIntegrationFor(t, accountID)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/risk", cookie))
	require.Contains(t, reasonCodes(t, body), "collateral_unavailable")
	require.Equal(t, "unreliable", body["freshness"].(map[string]any)["status"])
	require.Empty(t, integrationsOf(t, body),
		"an integration with no capture must not appear with a buffer of zero")
}

// Step 3: funding is summed per symbol over the window, from the ledger, and agrees with the
// events it names. Signed, because funding received and funding paid are different things.
func TestFundingSumsPerSymbolOverTheWindow(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("funding"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	instrumentID, symbol, settle := seedPerp(t)

	now := time.Now().UTC().Truncate(time.Second)
	appendFunding(t, accountID, integrationID, instrumentID, "-3", 1, now.Add(-6*time.Hour))
	appendFunding(t, accountID, integrationID, instrumentID, "1", 2, now.Add(-2*time.Hour))
	// Outside the window asked for, so it must not be counted.
	appendFunding(t, accountID, integrationID, instrumentID, "-100", 3, now.Add(-48*time.Hour))

	from := now.Add(-24 * time.Hour).Format(time.RFC3339)
	body := decodeJSON(t, do(t, http.MethodGet,
		srv.URL+"/funding?from="+from, cookie))
	requireMoneyIsString(t, body, "funding")

	rows := body["funding"].([]any)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)
	require.Equal(t, symbol, row["symbol"])
	require.Equal(t, settle, row["asset"])
	require.Equal(t, "-2", row["total"], "-3 paid plus 1 received, and nothing from outside")
	require.EqualValues(t, 2, row["events"])
}

// M5'S EXIT CRITERION, end to end: a funded futures account with one perp position reports a
// liquidation distance that moves when the mark moves, and a buffer that falls when the
// position grows.
//
// Both halves matter. A distance that never moved would be a stored constant rendered
// convincingly, and a buffer that never fell would be an account that looks equally safe at
// every size -- which is the one screen a leveraged trader must never be shown.
func TestTheDistanceMovesWithTheMarkAndTheBufferFallsAsThePositionGrows(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("m5-exit"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	instrumentID, symbol, _ := seedPerp(t)

	at := time.Now().UTC().Add(-20 * time.Second).Truncate(time.Millisecond)
	capture(t, accountID, integrationID, instrumentID, symbol, at,
		"1000", "250", "50000", "40000", "25000")
	before := integrationsOf(t, decodeJSON(t,
		do(t, http.MethodGet, srv.URL+"/risk", cookie)))[0].(map[string]any)

	// The mark falls towards the liquidation price, and the position doubles.
	capture(t, accountID, integrationID, instrumentID, symbol, at.Add(10*time.Second),
		"1000", "500", "45000", "40000", "50000")
	after := integrationsOf(t, decodeJSON(t,
		do(t, http.MethodGet, srv.URL+"/risk", cookie)))[0].(map[string]any)

	beforeDistance := decimal.RequireFromString(
		before["positions"].([]any)[0].(map[string]any)["liquidation_distance"].(string))
	afterDistance := decimal.RequireFromString(
		after["positions"].([]any)[0].(map[string]any)["liquidation_distance"].(string))
	require.True(t, afterDistance.LessThan(beforeDistance),
		"the mark moved towards liquidation and the distance did not: %s then %s",
		beforeDistance, afterDistance)

	beforeBuffer := decimal.RequireFromString(before["margin_buffer"].(string))
	afterBuffer := decimal.RequireFromString(after["margin_buffer"].(string))
	require.True(t, afterBuffer.LessThan(beforeBuffer),
		"the position grew and the buffer did not fall: %s then %s", beforeBuffer, afterBuffer)
}

// A position the venue reports no liquidation price for renders NO distance, not a distance
// of zero. Absent and "0% away" are opposite claims, and the field that would carry the
// second is the one a reader panics at.
func TestAPositionWithNoLiquidationPriceRendersNoDistance(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("no-liquidation"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	instrumentID, symbol, _ := seedPerp(t)

	capture(t, accountID, integrationID, instrumentID, symbol,
		time.Now().UTC().Add(-5*time.Second), "1000", "250", "50000", "0", "25000")

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/risk", cookie))
	p := integrationsOf(t, body)[0].(map[string]any)["positions"].([]any)[0].(map[string]any)
	require.Equal(t, "", p["liquidation_distance"])
}
