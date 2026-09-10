//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func createRule(
	t *testing.T, srv *httptest.Server, cookie *http.Cookie,
	metric, comparator, trigger, clear string,
) string {
	t.Helper()
	resp := send(t, http.MethodPost, srv.URL+"/alert-rules", fmt.Sprintf(
		`{"name":"rule-%s","metric":%q,"scope_kind":"portfolio","comparator":%q,
		  "trigger":%q,"clear":%q,"cooldown_seconds":0}`,
		metric+comparator+trigger, metric, comparator, trigger, clear), cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return decodeJSON(t, resp)["id"].(string)
}

// A rule goes in and comes back with both of its thresholds intact, as strings.
func TestAnAlertRuleRoundTrips(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("alert-rule"))

	id := createRule(t, srv, cookie, "leverage", "above", "3", "2.5")

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/alert-rules", cookie))
	rules := body["rules"].([]any)
	require.Len(t, rules, 1)
	rule := rules[0].(map[string]any)
	require.Equal(t, id, rule["id"])
	require.Equal(t, "3", rule["trigger"])
	require.Equal(t, "2.5", rule["clear"])
	require.Equal(t, false, rule["firing"])
}

// A band pointing the wrong way makes the rule fire and clear on the same evaluation. The
// schema refuses it; the API turns that into a sentence rather than a 500.
func TestARuleWithAnInvertedBandIsRefusedWithAnExplanation(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("inverted-band"))

	resp := send(t, http.MethodPost, srv.URL+"/alert-rules",
		`{"name":"backwards","metric":"leverage","scope_kind":"portfolio",
		  "comparator":"above","trigger":"3","clear":"4","cooldown_seconds":0}`, cookie)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestRetuningARuleReplacesBothThresholds(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("retune"))
	id := createRule(t, srv, cookie, "leverage", "above", "3", "2.5")

	resp := send(t, http.MethodPut, srv.URL+"/alert-rules/"+id,
		`{"name":"tighter","comparator":"above","trigger":"2","clear":"1.8",
		  "cooldown_seconds":600,"enabled":false}`, cookie)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	rule := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/alert-rules", cookie))["rules"].([]any)[0].(map[string]any)
	require.Equal(t, "tighter", rule["name"])
	require.Equal(t, "2", rule["trigger"])
	require.Equal(t, "1.8", rule["clear"])
	require.Equal(t, false, rule["enabled"])
}

// Someone else's rule is not found rather than forbidden: saying it exists confirms another
// account's data by its identifier.
func TestARuleOfAnotherAccountCannotBeRetuned(t *testing.T) {
	srv := newServer(t)
	stranger := register(t, srv, uniqueEmail("rule-owner"))
	id := createRule(t, srv, stranger, "leverage", "above", "3", "2.5")

	thief := register(t, srv, uniqueEmail("rule-thief"))
	resp := send(t, http.MethodPut, srv.URL+"/alert-rules/"+id,
		`{"name":"stolen","comparator":"above","trigger":"1","clear":"0.5",
		  "cooldown_seconds":0,"enabled":true}`, thief)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// L13: the secret goes in and never comes back. Not redacted, not partial -- absent. A
// channel secret is a standing permission to speak as this account's alerting system.
func TestAChannelSecretNeverComesBackOut(t *testing.T) {
	const hook = "https://hooks.example.test/T000/B111/XXXXXXXXXXXXXXXX"
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("channel-secret"))
	accountID := accountOf(t, srv, cookie)

	resp := send(t, http.MethodPost, srv.URL+"/alert-channels",
		fmt.Sprintf(`{"kind":"webhook","label":"ops","url":%q}`, hook), cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	listing := do(t, http.MethodGet, srv.URL+"/alert-channels", cookie)
	raw := readAll(t, listing)
	require.Contains(t, raw, "ops")
	require.NotContains(t, raw, hook, "the webhook URL came back out of the API")
	require.NotContains(t, raw, "XXXXXXXXXXXXXXXX")

	// And it is not sitting in the table in the clear either.
	var stored []byte
	require.NoError(t, tenancy.InTxRaw(context.Background(), appPool(t), accountID,
		func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(),
				`SELECT config_ciphertext FROM alert_channels`).Scan(&stored)
		}))
	require.NotContains(t, string(stored), hook)
}

func TestDeletingAChannelStopsItBeingListed(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("channel-delete"))

	created := send(t, http.MethodPost, srv.URL+"/alert-channels",
		`{"kind":"telegram","label":"phone","token":"123:ABC","chat_id":"42"}`, cookie)
	id := decodeJSON(t, created)["id"].(string)

	resp := send(t, http.MethodDelete, srv.URL+"/alert-channels/"+id, "", cookie)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/alert-channels", cookie))
	require.Empty(t, body["channels"])

	_ = store.New
}

// readAll reads a response body as text, so an assertion can be made about the WHOLE payload
// rather than about the fields a test remembered to look at. A secret that leaked through a
// field nobody thought to check is exactly the leak that gets shipped.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(raw)
}
