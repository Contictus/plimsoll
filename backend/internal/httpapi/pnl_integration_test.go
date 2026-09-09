//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func pnlURL(base string, from, to time.Time) string {
	return fmt.Sprintf("%s/pnl?from=%s&to=%s", base,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
}

// Realized comes from the ledger and needs no price at all, so it is exact and it is per
// quote asset. The interval below spans every fill the seed made, and the seed never sold
// anything -- so the honest answer is zero realized and an unrealized figure that is not.
func TestPnLIsRealizedFromTheLedgerAndUnrealizedFromEachEnd(t *testing.T) {
	_, cookie, srv, instrumentID := fundedAccountAt(t, "pnl")

	now := time.Now().UTC()
	// A price before the fills and a price after them: the open book is worth different
	// things at the two ends, and only the second end has a position to mark.
	seedPrice(t, instrumentID, "150", now.Add(-3*time.Hour))
	seedPrice(t, instrumentID, "200", now.Add(-30*time.Minute))

	from := now.Add(-90 * time.Minute) // after the fills, which the seed puts an hour back
	body := decodeJSON(t, do(t, http.MethodGet, pnlURL(srv.URL, from, now), cookie))
	requireMoneyIsString(t, body, "pnl")

	realized := body["realized_by_quote_asset"].([]any)
	require.Len(t, realized, 1)
	require.Equal(t, "0", realized[0].(map[string]any)["realized_pnl"],
		"nothing was closed out in this interval, and a zero here is a fact rather than a gap")

	// 4 bought at an average of 150: worth nothing more at 150, and 200 more at 200.
	require.Equal(t, "0", body["unrealized_usd_at_from"])
	require.Equal(t, "200", body["unrealized_usd_at_to"])
	require.Equal(t, "1000", body["total_value_usd_at_from"], "4 at 150, plus 400 of the quote")
	require.Equal(t, "1200", body["total_value_usd_at_to"])

	// Two runs, one per end. L10 forbids one number built from two price sources; an
	// interval is two questions and each answer names which end it came from.
	require.Equal(t, true, body["valuation_at_from"].(map[string]any)["rebuilt"])
	require.Equal(t, true, body["valuation_at_to"].(map[string]any)["rebuilt"])
	require.NotEqual(t, body["valuation_at_from"].(map[string]any)["as_of"],
		body["valuation_at_to"].(map[string]any)["as_of"])
}

// An interval that ends before it begins, or ends in the future, is refused rather than
// answered with a negative one.
func TestPnLRefusesAnImpossibleInterval(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("pnl-bad"))
	now := time.Now().UTC()

	for _, tc := range []struct {
		name     string
		from, to time.Time
	}{
		{"to before from", now.Add(-time.Hour), now.Add(-2 * time.Hour)},
		{"to in the future", now.Add(-time.Hour), now.Add(time.Hour)},
		{"zero length", now.Add(-time.Hour), now.Add(-time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, http.MethodGet, pnlURL(srv.URL, tc.from, tc.to), cookie)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
		})
	}
}
