//go:build integration

package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// atURL builds the request a client makes for a past instant.
func atURL(base string, at time.Time) string {
	return base + "/portfolio?at=" + at.UTC().Format(time.RFC3339)
}

// An account is empty before its first event. The failure this rules out is the easy one to
// ship: reading the live projection and stamping an old timestamp on it, which answers every
// question about the past with today's holdings.
func TestAtBeforeTheFirstEventIsAnEmptyPortfolio(t *testing.T) {
	accountID, cookie, srv := seededAccountAt(t, "at-empty")
	_ = accountID

	// The seed's events are an hour old; this is a day before any of them.
	before := time.Now().UTC().Add(-24 * time.Hour)
	resp := do(t, http.MethodGet, atURL(srv.URL, before), cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(readBody(t, resp))
	}
	body := decodeJSON(t, resp)
	requireMoneyIsString(t, body, "portfolio-at")

	require.Empty(t, body["positions"], "nothing had happened yet")
	require.Empty(t, body["balances"])
	require.Equal(t, before.Format(time.RFC3339), asOfOf(t, body).Format(time.RFC3339),
		"as_of is the instant asked about")

	live := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))
	require.NotEmpty(t, live["positions"], "and the same account does hold something now")
}

func asOfOf(t *testing.T, body map[string]any) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, body["as_of"].(string))
	require.NoError(t, err)
	return at.UTC()
}

// THE LOOK-AHEAD TEST.
//
// Asked about an instant inside a gap in price_ticks, the answer uses the last price before
// it and reports the gap. Reaching forward to the price after the gap would value the past
// with information nobody had at the time -- the bug that makes a backtest look brilliant and
// a liquidation arrive unannounced.
func TestAtInsideAPriceGapUsesTheOlderPriceAndReportsIt(t *testing.T) {
	_, cookie, srv, instrumentID := fundedAccountAt(t, "at-gap")

	// The seed's fills are an hour old, so both prices are after them and the position is
	// the same at every instant below. Only the prices differ.
	now := time.Now().UTC()
	seedPrice(t, instrumentID, "200", now.Add(-3*time.Hour))
	seedPrice(t, instrumentID, "999", now.Add(-2*time.Minute))

	body := decodeJSON(t, do(t, http.MethodGet, atURL(srv.URL, now.Add(-20*time.Minute)), cookie))

	// 4 of the base at 200, plus the 400 of the quote left over at its assumed dollar.
	require.Equal(t, "1200", body["total_value_usd"],
		"the price after the gap must not reach backwards into an answer about before it")
	require.Contains(t, freshnessCodes(t, body), "price_stale",
		"the newest price at that instant was hours old, and the response says so rather"+
			" than borrowing the fresher one from after the gap")

	run := body["valuation"].(map[string]any)
	require.Equal(t, true, run["rebuilt"], "a run for a past instant is reconstructed, not recorded")
	require.Equal(t, float64(0), run["run_id"])
}

// seededAccountAt stands up a server whose peg is the quote asset of the pair it seeds, so
// a rebuilt run has somewhere to terminate. The pair is unique per test, which is why the
// peg cannot be a constant.
func seededAccountAt(t *testing.T, name string) (uuid.UUID, *http.Cookie, *httptest.Server) {
	t.Helper()
	accountID, cookie, srv, _, _ := seededAccountWithPair(t, name)
	return accountID, cookie, srv
}

func seededAccountWithPair(
	t *testing.T, name string,
) (uuid.UUID, *http.Cookie, *httptest.Server, uuid.UUID, int64) {
	t.Helper()
	// The account is registered against a throwaway server, then the real one is built with
	// the seeded quote asset as its peg: the pair has to exist before the peg can name it.
	bootstrap := newServer(t)
	cookie := register(t, bootstrap, uniqueEmail(name))
	accountID := accountOf(t, bootstrap, cookie)
	integrationID, instrumentID, _, quote := seedFoldedPosition(t, accountID)

	srv := newServerWithPegs(t, quote)
	return accountID, cookie, srv, integrationID, instrumentID
}

// fundedAccountAt is the same with the deposit that pays for the fills, so the balances are
// possible and the negative-balance check stays quiet.
func fundedAccountAt(
	t *testing.T, name string,
) (uuid.UUID, *http.Cookie, *httptest.Server, int64) {
	t.Helper()
	accountID, cookie, srv, integrationID, instrumentID := seededAccountWithPair(t, name)
	appendDeposit(t, accountID, integrationID, quoteAssetOf(t, instrumentID), "1000")
	_, err := projection.Project(context.Background(), appPool(t), accountID, integrationID)
	require.NoError(t, err)
	return accountID, cookie, srv, instrumentID
}

// The same instant twice is the same answer, byte for byte. Without it "?at=" would be a
// number that moves for reasons nobody can name, and a lineage that cannot be reproduced is
// not a lineage (L3).
func TestTheSameInstantTwiceIsByteIdentical(t *testing.T) {
	_, cookie, srv, instrumentID := fundedAccountAt(t, "at-stable")

	now := time.Now().UTC()
	seedPrice(t, instrumentID, "200", now.Add(-3*time.Hour))
	at := now.Add(-10 * time.Minute)

	first := readBody(t, do(t, http.MethodGet, atURL(srv.URL, at), cookie))
	second := readBody(t, do(t, http.MethodGet, atURL(srv.URL, at), cookie))
	require.Equal(t, first, second)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(raw)
}

// A future instant is refused. Serving today's portfolio with tomorrow's timestamp is a
// claim about data that does not exist yet, and it is the same look-ahead in a friendlier
// costume.
func TestAtInTheFutureIsRefused(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("at-future"))

	resp := do(t, http.MethodGet, atURL(srv.URL, time.Now().UTC().Add(time.Hour)), cookie)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	resp = do(t, http.MethodGet, srv.URL+"/portfolio?at=yesterday", cookie)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
}
