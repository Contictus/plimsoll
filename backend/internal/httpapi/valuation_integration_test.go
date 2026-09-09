//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// seedPrice files one price for an instrument in the minute it was observed. It writes the
// same shape of row the recorder writes, so this test exercises the path a live feed fills
// rather than a shortcut only tests know about.
func seedPrice(t *testing.T, instrumentID int64, price string, observedAt time.Time) {
	t.Helper()
	_, err := ownerPool(t).Exec(context.Background(),
		`INSERT INTO price_ticks (instrument_id, ts, price, source, observed_at)
		 VALUES ($1, $2, $3, 'test', $4)`,
		instrumentID, observedAt.UTC().Truncate(time.Minute),
		decimal.RequireFromString(price), observedAt.UTC())
	require.NoError(t, err)
}

// produceRun values the registry as of an instant, with one asset assumed to be worth a
// dollar so the walk has somewhere to terminate.
func produceRun(t *testing.T, pegAssetID int64, asOf time.Time) valuation.Result {
	t.Helper()
	out, err := valuation.Produce(context.Background(), store.New(ownerPool(t)),
		"test:spot", asOf.UTC(),
		valuation.PegSet{pegAssetID: decimal.RequireFromString("1")})
	require.NoError(t, err)
	return out
}

func baseAssetOf(t *testing.T, instrumentID int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`SELECT base_asset_id FROM instruments WHERE id = $1`, instrumentID).Scan(&id))
	return id
}

// M4's exit criterion at the edge: events in, one run behind them, and a total that is the
// sum of what is held -- with as_of naming the run that produced it (K11, L10).
func TestPortfolioTotalIsTheValuedBalancesAndNamesItsRun(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("total"))
	accountID := accountOf(t, srv, cookie)
	integrationID, instrumentID, _, _ := seedFoldedPosition(t, accountID)

	// Funded first, so the balances are possible: the fills spend 600 of the 1000 deposited
	// and leave 4 of the base behind.
	quoteAssetID := quoteAssetOf(t, instrumentID)
	appendDeposit(t, accountID, integrationID, quoteAssetID, "1000")
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	at := time.Now().UTC()
	seedPrice(t, instrumentID, "200", at.Add(-time.Minute))
	produceRun(t, quoteAssetID, at)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))
	requireMoneyIsString(t, body, "portfolio")

	// 4 of the base at 200, plus the 400 of the quote still unspent, at its assumed dollar.
	require.Equal(t, "1200", body["total_value_usd"])
	require.Empty(t, body["unpriced_assets"])

	run := body["valuation"].(map[string]any)
	require.Equal(t, "test:spot", run["price_source"])
	require.Equal(t, "USD", run["numeraire"])
	require.Equal(t, run["as_of"], body["as_of"],
		"as_of is the run's instant, not the moment the response was serialized (L10)")

	p := body["positions"].([]any)[0].(map[string]any)
	require.Equal(t, "800", p["market_value"], "4 at 200")
	require.Equal(t, "200", p["unrealized_pnl"], "4 bought at 150, marked at 200")

	require.NotContains(t, freshnessCodes(t, body), "valuation_unavailable",
		"a run exists, so the reason that says none has completed must be gone")
	require.NotContains(t, freshnessCodes(t, body), "price_stale",
		"the price was observed a minute ago; a warning here would be one a reader learns to ignore")
	require.Contains(t, freshnessCodes(t, body), "assumed_peg",
		"the quote asset was assumed rather than traded, and the response says so (K17)")

	// The only thing wrong with this response is that no worker is reading the seeded
	// integration, which is true and is the seed's doing. Naming it rather than asserting a
	// status keeps this test about the valuation: a new error introduced by the marking
	// would fail here instead of hiding behind an "unreliable" that was already expected.
	for _, r := range body["freshness"].(map[string]any)["reasons"].([]any) {
		reason := r.(map[string]any)
		if reason["severity"] == "error" {
			require.Equal(t, "ingest_stalled", reason["code"], "detail: %v", reason["detail"])
		}
	}
}

// The lineage's other half. The steps say what was traded; the prices say what it is worth
// and how the walk got there. Multiplying a path's rates must reproduce its price exactly --
// not to within a rounding error, which is not a proof (K11, K17).
func TestLineageCarriesThePricePathItWasValuedThrough(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("lineage-prices"))
	accountID := accountOf(t, srv, cookie)
	integrationID, instrumentID, _, _ := seedFoldedPosition(t, accountID)

	quoteAssetID := quoteAssetOf(t, instrumentID)
	appendDeposit(t, accountID, integrationID, quoteAssetID, "1000")
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	at := time.Now().UTC()
	seedPrice(t, instrumentID, "200", at.Add(-time.Minute))
	produceRun(t, quoteAssetID, at)

	list := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions", cookie))
	id := list["positions"].([]any)[0].(map[string]any)["id"].(string)
	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions/"+id+"/lineage", cookie))
	requireMoneyIsString(t, body, "lineage")

	prices := body["prices"].([]any)
	require.Len(t, prices, 2, "the base and the quote, in that order")

	base := prices[0].(map[string]any)
	require.Equal(t, float64(baseAssetOf(t, instrumentID)), base["asset_id"])
	require.Equal(t, "200", base["price_usd"], "one hop through the seeded pair at a pegged quote")

	// The audit property: the hops multiply out to the price that was served.
	product := decimal.RequireFromString("1")
	hops := base["path"].([]any)
	require.NotEmpty(t, hops)
	for _, h := range hops {
		product = product.Mul(decimal.RequireFromString(h.(map[string]any)["rate"].(string)))
	}
	require.Equal(t, base["price_usd"], product.String(),
		"a path a reader cannot multiply out is a number nobody can check")

	quote := prices[1].(map[string]any)
	require.Equal(t, float64(quoteAssetID), quote["asset_id"])
	require.Equal(t, true, quote["assumed_peg"], "the quote asset is the peg this run terminates on")
}
