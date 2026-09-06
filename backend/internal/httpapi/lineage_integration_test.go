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
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/shopspring/decimal"

	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// M3's other exit criterion: a position opened down to the events that produced it, with
// the state each one left behind. This is the product thesis in one assertion -- the
// numbers are right, and here is the arithmetic (ARCHITECTURE.md section 10, rule 6).
func TestLineageOpensAPositionDownToItsEvents(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("lineage"))
	accountID := accountOf(t, srv, cookie)
	seedFoldedPosition(t, accountID)

	list := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions", cookie))
	id := list["positions"].([]any)[0].(map[string]any)["id"].(string)

	resp := do(t, http.MethodGet, srv.URL+"/positions/"+id+"/lineage", cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSON(t, resp)
	requireMoneyIsString(t, body, "lineage")

	require.Equal(t, float64(2), body["total_events"])
	steps := body["steps"].([]any)
	require.Len(t, steps, 2)

	first := steps[0].(map[string]any)
	require.Equal(t, "2", first["event"].(map[string]any)["quantity"])
	require.Equal(t, "100", first["event"].(map[string]any)["price"])
	require.Equal(t, "100", first["resulting"].(map[string]any)["avg_entry_price"],
		"after one fill at 100 the average entry is 100")

	second := steps[1].(map[string]any)
	require.Equal(t, "4", second["resulting"].(map[string]any)["quantity"])
	require.Equal(t, "150", second["resulting"].(map[string]any)["avg_entry_price"],
		"2 at 100 then 2 at 200 averages to 150, and the lineage shows the step that did it")

	// The last step must reproduce the stored position, or the endpoint has just disproved
	// the number it is serving.
	require.Equal(t, body["position"].(map[string]any)["quantity"],
		second["resulting"].(map[string]any)["quantity"])
	require.Equal(t, body["position"].(map[string]any)["avg_entry_price"],
		second["resulting"].(map[string]any)["avg_entry_price"])

	// L15: the exchange payload is kept forever, and this is where it earns that.
	require.NotNil(t, first["raw"])

	var codes []string
	for _, r := range body["freshness"].(map[string]any)["reasons"].([]any) {
		codes = append(codes, r.(map[string]any)["code"].(string))
	}
	require.NotContains(t, codes, "lineage_mismatch",
		"the replay and the projection must agree when they end on the same event")
}

// THE TEST THE WHOLE PRODUCT CLAIM RESTS ON.
//
// If the events do not reproduce the stored number, the response says so rather than
// serving a number it has just disproved (L11). The corruption here is written directly
// into the projection as the owner -- which is the only way to produce it, because nothing
// in the system has a path to write positions except the fold.
func TestALineageThatDisagreesWithTheProjectionSaysSo(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("mismatch"))
	accountID := accountOf(t, srv, cookie)
	integrationID, instrumentID, _, _ := seedFoldedPosition(t, accountID)

	list := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions", cookie))
	id := list["positions"].([]any)[0].(map[string]any)["id"].(string)

	// positions carries FORCE ROW LEVEL SECURITY, so even the owner binds the account.
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE positions SET quantity = quantity + 1
			 WHERE integration_id = $1 AND instrument_id = $2`, integrationID, instrumentID)
		return err
	}))

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions/"+id+"/lineage", cookie))

	var mismatch map[string]any
	for _, r := range body["freshness"].(map[string]any)["reasons"].([]any) {
		if r.(map[string]any)["code"] == "lineage_mismatch" {
			mismatch = r.(map[string]any)
		}
	}
	require.NotNil(t, mismatch, "a projection the events do not reproduce was served silently")
	require.Equal(t, "error", mismatch["severity"])
	require.Equal(t, "unreliable", body["freshness"].(map[string]any)["status"])
	require.Contains(t, mismatch["detail"], "5", "the detail must name both numbers")
}

// Another account's lineage is 404, the same answer as a position that does not exist.
func TestAnotherAccountsLineageIsNotFound(t *testing.T) {
	srv := newServer(t)

	mine := register(t, srv, uniqueEmail("lineage-mine"))
	seedFoldedPosition(t, accountOf(t, srv, mine))
	list := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions", mine))
	id := list["positions"].([]any)[0].(map[string]any)["id"].(string)

	theirs := register(t, srv, uniqueEmail("lineage-theirs"))
	resp := do(t, http.MethodGet, srv.URL+"/positions/"+id+"/lineage", theirs)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	bad := do(t, http.MethodGet, srv.URL+"/positions/"+uuid.NewString()+"/lineage", mine)
	require.Equal(t, http.StatusBadRequest, bad.StatusCode)
}

// The ledger, paginated. Never on seq: identity values are assigned before commit, so a
// seq cursor can skip a row that was still in flight (K20, K41). seq is still in the
// response, because it is what a support conversation quotes.
func TestTransactionsPageInCanonicalOrder(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("ledger"))
	accountID := accountOf(t, srv, cookie)
	seedFoldedPosition(t, accountID)

	first := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/transactions?limit=1", cookie))
	requireMoneyIsString(t, first, "transactions")

	events := first["events"].([]any)
	require.Len(t, events, 1)
	require.Equal(t, "2", events[0].(map[string]any)["quantity"])
	require.Equal(t, "100", events[0].(map[string]any)["price"])
	require.NotEmpty(t, events[0].(map[string]any)["venue_event_id"])
	require.NotZero(t, events[0].(map[string]any)["seq"])

	cursor := first["next_cursor"].(string)
	require.NotEmpty(t, cursor, "a full page must offer a way to continue")

	second := decodeJSON(t, do(t,
		http.MethodGet, srv.URL+"/transactions?limit=1&cursor="+cursor, cookie))
	next := second["events"].([]any)
	require.Len(t, next, 1)
	require.Equal(t, "200", next[0].(map[string]any)["price"],
		"the second page must be the next event, not the first one again")

	// A short page is the end of what exists now, and offering a cursor there would invite
	// a client to poll it.
	last := decodeJSON(t, do(t,
		http.MethodGet, srv.URL+"/transactions?limit=1&cursor="+second["next_cursor"].(string),
		cookie))
	require.Empty(t, last["events"])
	require.Empty(t, last["next_cursor"])
}

// A corrupted cursor is rejected rather than silently restarting at page one, which would
// make a client loop forever without ever being told.
func TestAMalformedCursorIsRejected(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("bad-cursor"))

	for _, c := range []string{"!!!not-base64!!!", "bm90LWEtY3Vyc29y"} {
		resp := do(t, http.MethodGet, srv.URL+"/transactions?cursor="+c, cookie)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "cursor %q", c)
	}
}

// A projection that is behind is not a disagreement. The fold runs on a ticker (K38), so
// the replay is routinely ahead of the stored row -- and calling that a mismatch would make
// the one serious signal in this response fire constantly and stop meaning anything.
func TestAProjectionMerelyBehindIsNotCalledADisagreement(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("behind"))
	accountID := accountOf(t, srv, cookie)
	integrationID, instrumentID, _, _ := seedFoldedPosition(t, accountID)

	list := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions", cookie))
	id := list["positions"].([]any)[0].(map[string]any)["id"].(string)

	// A third fill, appended and deliberately not folded.
	appendUnfoldedFill(t, accountID, integrationID, instrumentID)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/positions/"+id+"/lineage", cookie))

	var codes []string
	for _, r := range body["freshness"].(map[string]any)["reasons"].([]any) {
		codes = append(codes, r.(map[string]any)["code"].(string))
	}
	require.NotContains(t, codes, "lineage_mismatch")
	require.Contains(t, codes, "projection_lagging",
		"the reader is still told the positions are behind the events")
	require.Equal(t, float64(3), body["total_events"],
		"the replay sees every event, including the one the fold has not reached")
}

// appendUnfoldedFill writes one more fill to the ledger without running the projector, the
// way a live event lands between two ticks.
func appendUnfoldedFill(t *testing.T, accountID, integrationID uuid.UUID, instrumentID int64) {
	t.Helper()
	ctx := context.Background()
	e := ledger.Event{
		AccountID:     accountID,
		IntegrationID: integrationID,
		VenueEventID:  fmt.Sprintf("spot:trade:%d:99", instrumentID),
		VenueSequence: 99,
		Source:        "stream",
		EventType:     ledger.TypeTrade,
		InstrumentID:  &instrumentID,
		Side:          ledger.SideBuy,
		Quantity:      decimal.NewNullDecimal(decimal.RequireFromString("1")),
		Price:         decimal.NewNullDecimal(decimal.RequireFromString("300")),
		EventTime:     time.Now().UTC().Truncate(time.Second),
		Raw:           json.RawMessage(`{}`),
	}
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := ledger.Append(ctx, q, []ledger.Event{e})
		return err
	}))
}
