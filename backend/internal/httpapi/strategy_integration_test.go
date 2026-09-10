//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// send is do() with a JSON body, for the two endpoints in M6 that take one.
func send(t *testing.T, method, url, body string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url,
		bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func createStrategy(t *testing.T, srv *httptest.Server, cookie *http.Cookie, name, kind string) string {
	t.Helper()
	resp := send(t, http.MethodPost, srv.URL+"/strategies",
		fmt.Sprintf(`{"name":%q,"kind":%q}`, name, kind), cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return decodeJSON(t, resp)["id"].(string)
}

// The tag is the one piece of user input in this system that decides how risk is grouped, so
// it goes in and comes back out through the API the user actually has -- not through the
// store.
func TestAPositionCanBeTaggedAndTheTagIsReadBack(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("tagging"))
	accountID := accountOf(t, srv, cookie)
	integrationID, instrumentID, symbol, _ := seedFoldedPosition(t, accountID)

	id := createStrategy(t, srv, cookie, "cash and carry", "basis")
	positionID := fmt.Sprintf("%s.%d", integrationID, instrumentID)

	resp := send(t, http.MethodPut, srv.URL+"/positions/"+positionID+"/strategy",
		fmt.Sprintf(`{"strategy_id":%q}`, id), cookie)
	// 204: the tag is set and there is nothing to say back that the caller did not send.
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	listing := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/strategies", cookie))
	strategies := listing["strategies"].([]any)
	require.Len(t, strategies, 1)
	require.Equal(t, "cash and carry", strategies[0].(map[string]any)["name"])
	require.EqualValues(t, 1, strategies[0].(map[string]any)["positions"],
		"the strategy does not know about the position that was just tagged")

	// And the position itself says which group it is in, because that is where a reader
	// looking at one number asks the question.
	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions/"+positionID, cookie))
	position := body["position"].(map[string]any)
	require.Equal(t, symbol, position["symbol"])
	require.Equal(t, "cash and carry", position["strategy"])
	require.Equal(t, id, position["strategy_id"])
}

// A tag naming a strategy that belongs to someone else must be refused, not silently stored.
// The composite foreign key makes it impossible at the storage layer; this asserts the API
// turns that into an answer rather than a 500.
func TestAPositionCannotBeTaggedWithAnotherAccountsStrategy(t *testing.T) {
	srv := newServer(t)

	stranger := register(t, srv, uniqueEmail("stranger"))
	strangerStrategy := createStrategy(t, srv, stranger, "their thesis", "basis")

	cookie := register(t, srv, uniqueEmail("tag-thief"))
	accountID := accountOf(t, srv, cookie)
	integrationID, instrumentID, _, _ := seedFoldedPosition(t, accountID)
	positionID := fmt.Sprintf("%s.%d", integrationID, instrumentID)

	resp := send(t, http.MethodPut, srv.URL+"/positions/"+positionID+"/strategy",
		fmt.Sprintf(`{"strategy_id":%q}`, strangerStrategy), cookie)
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"another account's strategy must be unknown, not forbidden: telling a caller it"+
			" exists is itself a leak")
}

// Two strategies of one account may not share a name: the name is what an alert says, and two
// of them would make the message ambiguous about which book it came from.
func TestAStrategyNameIsUniquePerAccount(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("dup-name"))
	createStrategy(t, srv, cookie, "basis", "basis")

	resp := send(t, http.MethodPost, srv.URL+"/strategies", `{"name":"basis","kind":"basis"}`, cookie)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

// The position is unknown while its integration is REAL and the instrument EXISTS. A test
// that used a random integration id would pass without the position check ever running: the
// composite foreign key to `integrations` would reject the row on its own, and the endpoint
// would answer 404 for a reason that says nothing about whether the position was there.
func TestAnUnknownPositionCannotBeTagged(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("ghost-position"))
	accountID := accountOf(t, srv, cookie)
	integrationID, _, _, _ := seedFoldedPosition(t, accountID)
	untraded, _, _ := seedPair(t)

	id := createStrategy(t, srv, cookie, "directional", "directional")
	resp := send(t, http.MethodPut,
		fmt.Sprintf("%s/positions/%s.%d/strategy", srv.URL, integrationID, untraded),
		fmt.Sprintf(`{"strategy_id":%q}`, id), cookie)
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"a position that was never folded can be tagged; the tag would sit there until it"+
			" appeared and then apply to it silently")
}
