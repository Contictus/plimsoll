//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func freshnessCodes(t *testing.T, body map[string]any) []string {
	t.Helper()
	var out []string
	for _, r := range body["freshness"].(map[string]any)["reasons"].([]any) {
		out = append(out, r.(map[string]any)["code"].(string))
	}
	return out
}

func balanceOf(t *testing.T, body map[string]any, asset string) map[string]any {
	t.Helper()
	for _, b := range body["balances"].([]any) {
		if b.(map[string]any)["asset"] == asset {
			return b.(map[string]any)
		}
	}
	t.Fatalf("no balance for %s in %v", asset, body["balances"])
	return nil
}

// M3 shipped a portfolio made of positions, which is the right answer for a derivatives
// account and half of one for a spot account: "long 0.5 BTC at 60000" does not say how much
// USDT is left. Both come from the same events.
func TestThePortfolioReportsWhatIsActuallyHeld(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("balances"))
	accountID := accountOf(t, srv, cookie)
	_, _, _, quote := seedFoldedPosition(t, accountID)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))
	requireMoneyIsString(t, body, "portfolio")

	// Two fills: 2 at 100 then 2 at 200, so 600 of the quote is gone and 4 of the base
	// arrived. A fold that only credited the base would show free money.
	require.Equal(t, "-600", balanceOf(t, body, quote)["quantity"])

	// And -600 is impossible, because this seed never deposited anything. The check that
	// says so is K14's strongest, and it found a missing event without being told what the
	// missing event was.
	require.Equal(t, true, balanceOf(t, body, quote)["negative"])
	require.Contains(t, freshnessCodes(t, body), "negative_balance")
	require.Equal(t, "unreliable", body["freshness"].(map[string]any)["status"])
}

// With the deposit that pays for them, the same fills leave a balance that is possible --
// and the check goes quiet, which is the half that makes it worth having. A check that
// fires on every account is not a check.
func TestADepositThatPaysForTheFillsSilencesTheNegativeBalanceCheck(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("funded"))
	accountID := accountOf(t, srv, cookie)
	integrationID, instrumentID, _, quote := seedFoldedPosition(t, accountID)

	quoteAssetID := quoteAssetOf(t, instrumentID)
	appendDeposit(t, accountID, integrationID, quoteAssetID, "1000")
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))

	require.Equal(t, "400", balanceOf(t, body, quote)["quantity"],
		"1000 deposited, 600 spent")
	require.Equal(t, false, balanceOf(t, body, quote)["negative"])
	require.NotContains(t, freshnessCodes(t, body), "negative_balance")
}

// A fee paid in a ticker the registry does not cover is stored -- losing a fill to keep a
// fee is the worse trade -- and the balance for it is short by exactly that fee. The reader
// is told rather than left to find it in a reconciliation months later (K22, L11).
func TestAFeeInAnUnknownTickerIsReportedRatherThanSilentlyDropped(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("unknown-fee"))
	accountID := accountOf(t, srv, cookie)
	integrationID, _, _, _ := seedFoldedPosition(t, accountID)

	// seedFoldedPosition pays its fees in "BNB" and never resolves them: the normalizer is
	// not in this path, so fee_asset_id is NULL exactly as it would be for a coin the
	// registry does not cover.
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))
	require.Contains(t, freshnessCodes(t, body), "unknown_symbol")
}

func quoteAssetOf(t *testing.T, instrumentID int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`SELECT quote_asset_id FROM instruments WHERE id = $1`, instrumentID).Scan(&id))
	return id
}

func appendDeposit(t *testing.T, accountID, integrationID uuid.UUID, assetID int64, amount string) {
	t.Helper()
	ctx := context.Background()
	e := ledger.Event{
		AccountID:     accountID,
		IntegrationID: integrationID,
		VenueEventID:  fmt.Sprintf("spot:deposit:%d:1", assetID),
		VenueSequence: 1,
		Source:        "rest",
		EventType:     ledger.TypeDeposit,
		AssetID:       &assetID,
		Quantity:      decimal.NewNullDecimal(decimal.RequireFromString(amount)),
		// Before the fills, so the deposit pays for them rather than arriving after the
		// balance had already gone impossible.
		EventTime: time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second),
		Raw:       json.RawMessage(`{}`),
	}
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := ledger.Append(ctx, q, []ledger.Event{e})
		return err
	}))
}
