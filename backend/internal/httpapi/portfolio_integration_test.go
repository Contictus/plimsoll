//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// moneyFields are the response keys that carry money. Every one of them must arrive as a
// JSON string (L1): a float64 in a client's parser is our bug too, and the only way to make
// that impossible is to never send a number.
var moneyFields = map[string]bool{
	"quantity": true, "avg_entry_price": true, "cost_basis": true,
	"realized_pnl": true, "amount": true, "market_value": true, "unrealized_pnl": true,
	"price_usd": true, "value_usd": true, "total_value_usd": true,
}

// requireMoneyIsString walks the decoded response and fails on any money field that is not
// a string. It walks rather than checking a fixed list of paths, so a field added to a
// nested body later is covered by a test nobody has to remember to update.
func requireMoneyIsString(t *testing.T, node any, path string) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			if moneyFields[key] {
				_, ok := child.(string)
				require.True(t, ok, "%s.%s is %T; money crosses the API as a string (L1)",
					path, key, child)
			}
			requireMoneyIsString(t, child, path+"."+key)
		}
	case []any:
		for i, child := range v {
			requireMoneyIsString(t, child, fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// accountOf reads the caller's own id the way any client would, so the test never learns it
// through a back door the API does not have.
func accountOf(t *testing.T, srv *httptest.Server, cookie *http.Cookie) uuid.UUID {
	t.Helper()
	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/me", cookie))
	id, err := uuid.Parse(body["account_id"].(string))
	require.NoError(t, err)
	return id
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

func seedIntegrationFor(t *testing.T, accountID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'api-test')`, id, accountID)
		return err
	}))
	return id
}

func seedPair(t *testing.T) (instrumentID int64, symbol, quote string) {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)

	// Upper case, because a canonical symbol is: peg configuration names assets the way a
	// human writes a ticker and is normalized to upper case when it resolves them (K17).
	symbol = "AP-" + strings.ToUpper(uuid.NewString())
	quote = "AQ-" + strings.ToUpper(uuid.NewString())

	var baseID, quoteID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"AB-"+strings.ToUpper(uuid.NewString())).Scan(&baseID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'stablecoin') RETURNING id`,
		quote).Scan(&quoteID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id)
		 VALUES ($1, 'spot', $2, $3) RETURNING id`, symbol, baseID, quoteID).Scan(&instrumentID))
	return instrumentID, symbol, quote
}

// seedFoldedPosition puts one bought position in front of the API: two fills appended to
// the ledger, then folded. Nothing here writes `positions` directly -- an endpoint tested
// against hand-written projection rows would pass over a fold that never ran.
func seedFoldedPosition(t *testing.T, accountID uuid.UUID) (integrationID uuid.UUID, instrumentID int64, symbol, quote string) {
	t.Helper()
	ctx := context.Background()
	pool := appPool(t)

	integrationID = seedIntegrationFor(t, accountID)
	instrumentID, symbol, quote = seedPair(t)

	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for i, fill := range []struct{ qty, price string }{{"2", "100"}, {"2", "200"}} {
		seq := int64(i + 1)
		e := ledger.Event{
			AccountID:     accountID,
			IntegrationID: integrationID,
			VenueEventID:  fmt.Sprintf("spot:trade:%d:%d", instrumentID, seq),
			VenueSequence: seq,
			Source:        "rest",
			EventType:     ledger.TypeTrade,
			InstrumentID:  &instrumentID,
			Side:          ledger.SideBuy,
			Quantity:      decimal.NewNullDecimal(decimal.RequireFromString(fill.qty)),
			Price:         decimal.NewNullDecimal(decimal.RequireFromString(fill.price)),
			Fee:           decimal.NewNullDecimal(decimal.RequireFromString("0.25")),
			FeeAsset:      "BNB",
			EventTime:     at.Add(time.Duration(seq) * time.Second),
			Raw:           json.RawMessage(`{}`),
		}
		require.NoError(t, tenancy.InTx(ctx, pool, accountID, func(q *store.Queries) error {
			_, err := ledger.Append(ctx, q, []ledger.Event{e})
			return err
		}))
	}
	_, err := projection.Project(ctx, pool, accountID, integrationID)
	require.NoError(t, err)
	return integrationID, instrumentID, symbol, quote
}

// M3's exit criterion, end to end: events in, portfolio out, and every number the fold
// produced arriving as a string.
func TestPortfolioReportsTheFoldedPositionWithSubtotalsPerQuoteAsset(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("portfolio"))
	accountID := accountOf(t, srv, cookie)
	_, _, symbol, quote := seedFoldedPosition(t, accountID)

	resp := do(t, http.MethodGet, srv.URL+"/portfolio", cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSON(t, resp)
	requireMoneyIsString(t, body, "portfolio")

	positions := body["positions"].([]any)
	require.Len(t, positions, 1)
	p := positions[0].(map[string]any)
	require.Equal(t, symbol, p["symbol"])
	require.Equal(t, quote, p["quote_asset"])
	require.Equal(t, "4", p["quantity"])
	require.Equal(t, "150", p["avg_entry_price"])
	require.Equal(t, "600", p["cost_basis"])
	require.Equal(t, false, p["flat"])

	fees := p["fees"].([]any)
	require.Len(t, fees, 1)
	require.Equal(t, "BNB", fees[0].(map[string]any)["asset"])
	require.Equal(t, "0.5", fees[0].(map[string]any)["amount"],
		"the fee rides on its parent event and is never folded into the entry price (L9)")

	subtotals := body["subtotals_by_quote_asset"].([]any)
	require.Len(t, subtotals, 1)
	require.Equal(t, quote, subtotals[0].(map[string]any)["asset"])
	require.Equal(t, "600", subtotals[0].(map[string]any)["cost_basis"])
}

// A run whose prices cover none of what this account holds serves no total, and says why.
// The property is asserted here rather than "no run exists at all": a valuation run belongs
// to no account (00019), so any other test in this database producing one would make that
// version of this test pass or fail depending on the order the suite happened to run in.
//
// A client that found a bare 0 here would reasonably read it as an empty account, which is
// the confident-and-wrong answer the envelope exists to refuse (K11, L10, L11).
func TestPortfolioCarriesNoTotalAndSaysWhy(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("no-total"))
	accountID := accountOf(t, srv, cookie)
	seedFoldedPosition(t, accountID)

	// A run this account's assets are not in: the peg belongs to somebody else's pair, and
	// nothing prices the assets seeded above.
	otherInstrument, _, _ := seedPair(t)
	produceRun(t, quoteAssetOf(t, otherInstrument), time.Now().UTC())

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))

	require.Equal(t, "", body["total_value_usd"],
		"nothing here could be priced, so there is no total -- and never a zero")
	for _, banned := range []string{"total", "total_value", "equity"} {
		_, present := body[banned]
		require.False(t, present, "%s is not a field this API has", banned)
	}

	fresh := body["freshness"].(map[string]any)
	var codes []string
	for _, r := range fresh["reasons"].([]any) {
		codes = append(codes, r.(map[string]any)["code"].(string))
	}
	require.Contains(t, codes, "unknown_symbol",
		"an asset with no route to the numeraire is named, not dropped")
	require.Equal(t, "unreliable", fresh["status"])
	require.NotEmpty(t, body["as_of"], "every data response carries as_of (L10)")
}

// A position's id has to survive a rebuild. positions is a projection: L3 says it can be
// dropped and folded again, and a surrogate key would come back different -- breaking every
// link a user saved and every alert that named one. The natural key cannot (K42).
func TestAPositionIDSurvivesARebuild(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("stable-id"))
	accountID := accountOf(t, srv, cookie)
	integrationID, _, _, _ := seedFoldedPosition(t, accountID)

	before := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions", cookie))
	id := before["positions"].([]any)[0].(map[string]any)["id"].(string)

	require.NoError(t, projection.Rebuild(ctx, appPool(t), accountID, integrationID))

	after := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions", cookie))
	require.Equal(t, id, after["positions"].([]any)[0].(map[string]any)["id"],
		"a rebuild renamed the position")

	one := do(t, http.MethodGet, srv.URL+"/positions/"+id, cookie)
	require.Equal(t, http.StatusOK, one.StatusCode)
	require.Equal(t, id, decodeJSON(t, one)["position"].(map[string]any)["id"])
}

// Another account's position is 404, not 403: telling a caller that an id exists but is not
// theirs is telling them something about an account they cannot see.
func TestAnotherAccountsPositionIsNotFound(t *testing.T) {
	srv := newServer(t)

	mine := register(t, srv, uniqueEmail("mine"))
	seedFoldedPosition(t, accountOf(t, srv, mine))

	theirs := register(t, srv, uniqueEmail("theirs"))
	theirsBody := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions", theirs))
	require.Empty(t, theirsBody["positions"])

	mineBody := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions", mine))
	id := mineBody["positions"].([]any)[0].(map[string]any)["id"].(string)

	resp := do(t, http.MethodGet, srv.URL+"/positions/"+id, theirs)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// A malformed id is a client error, and it must not reach the database as one.
func TestAMalformedPositionIDIsRejected(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("bad-id"))

	for _, id := range []string{"nonsense", "not-a-uuid.7", uuid.NewString(), uuid.NewString() + ".x"} {
		resp := do(t, http.MethodGet, srv.URL+"/positions/"+id, cookie)
		require.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound}, resp.StatusCode,
			"id %q", id)
	}
}

// An account with no integrations at all still gets a well-formed response: empty lists
// rather than nulls, and a freshness that explains there is nothing behind them.
func TestAnEmptyAccountGetsListsRatherThanNulls(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("empty"))

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))

	require.NotNil(t, body["positions"])
	require.Empty(t, body["positions"])
	require.NotNil(t, body["subtotals_by_quote_asset"])
	require.NotNil(t, body["fees"])
}
