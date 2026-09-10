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

// appendTransfer moves one asset between two wallets of one integration and folds the
// result, exactly as the worker would.
func appendTransfer(
	t *testing.T, accountID, integrationID uuid.UUID, assetID int64,
	from, to, quantity string, seq int64, at time.Time,
) {
	t.Helper()
	ctx := context.Background()
	pool := appPool(t)

	e := ledger.Event{
		AccountID:     accountID,
		IntegrationID: integrationID,
		VenueEventID:  fmt.Sprintf("transfer:MAIN_UMFUTURE:%d", seq),
		VenueSequence: seq,
		Source:        "rest",
		EventType:     ledger.TypeTransfer,
		AssetID:       &assetID,
		TransferFrom:  from,
		TransferTo:    to,
		Quantity:      decimal.NewNullDecimal(decimal.RequireFromString(quantity)),
		EventTime:     at,
		Raw:           json.RawMessage(`{"type":"MAIN_UMFUTURE"}`),
	}
	require.NoError(t, tenancy.InTx(ctx, pool, accountID, func(q *store.Queries) error {
		_, err := ledger.Append(ctx, q, []ledger.Event{e})
		return err
	}))
	_, err := projection.Project(ctx, pool, accountID, integrationID)
	require.NoError(t, err)
}

// A transfer is history, and history the account can read. It folds to no delta, so it
// leaves no trace in any total -- which makes the ledger listing the ONLY place it exists
// for a reader. An endpoint that omitted it would produce a portfolio nobody could
// reconcile against their exchange statement.
func TestATransferIsVisibleInTheTransactionListing(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("transfer-listing"))
	accountID := accountOf(t, srv, cookie)
	integrationID, _, _, quote := seedFoldedPosition(t, accountID)
	quoteID := assetIDOf(t, quote)

	// After both fills, so canonical order puts it last.
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	appendTransfer(t, accountID, integrationID, quoteID, "spot", "usdm", "500", 3,
		at.Add(3*time.Second))

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/transactions", cookie))
	requireMoneyIsString(t, body, "transactions")

	events := body["events"].([]any)
	require.Len(t, events, 3, "the transfer is missing from the account's own ledger")

	last := events[2].(map[string]any)
	require.Equal(t, "TRANSFER", last["event_type"])
	require.Equal(t, "spot", last["transfer_from"])
	require.Equal(t, "usdm", last["transfer_to"])
	require.Equal(t, "500", last["quantity"])
	require.Equal(t, quote, last["asset"], "a transfer names an asset, never an instrument")
	require.Empty(t, last["instrument"])
	require.Empty(t, last["price"], "a transfer has no price")

	// The fills either side of it say nothing about wallets: the endpoints belong to
	// transfers, and a fill carrying one would be a normalizer bug the schema refuses.
	require.Empty(t, events[0].(map[string]any)["transfer_from"])
	require.Empty(t, events[0].(map[string]any)["transfer_to"])
}

// M3.5'S EXIT CRITERION, end to end and through HTTP: a spot to futures transfer is not
// counted as a sale.
//
// Four assertions and one sentence. Three of them are things that must NOT change -- the
// position, its realized PnL, and the balance -- and the fourth is that the transfer is
// nonetheless there. A milestone whose success is mostly the absence of change needs the
// fourth one, or a system that dropped the event on the floor would pass.
func TestASpotToFuturesTransferIsNotCountedAsASale(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("not-a-sale"))
	accountID := accountOf(t, srv, cookie)
	integrationID, _, _, quote := seedFoldedPosition(t, accountID)
	quoteID := assetIDOf(t, quote)

	before := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))

	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	appendTransfer(t, accountID, integrationID, quoteID, "spot", "usdm", "500", 3,
		at.Add(3*time.Second))

	after := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))

	require.Equal(t, positionsOf(before), positionsOf(after),
		"the transfer changed a position, or its realized PnL: it was read as a sale")
	require.Equal(t, balancesOf(before), balancesOf(after),
		"the transfer changed the balance; money that stayed inside the account was spent")

	// And it happened: the account can see the movement it just made.
	listing := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/transactions", cookie))
	var found bool
	for _, e := range listing["events"].([]any) {
		if e.(map[string]any)["event_type"] == "TRANSFER" {
			found = true
		}
	}
	require.True(t, found,
		"nothing changed because nothing was recorded, which is a different milestone")
}

// positionsOf and balancesOf lift the two halves a transfer must not disturb out of a
// portfolio response. Compared as whole sub-documents rather than field by field, so a
// number this test does not know about yet is still covered by it.
func positionsOf(body map[string]any) any { return body["positions"] }
func balancesOf(body map[string]any) any  { return body["balances"] }

// assetIDOf resolves a canonical symbol the way the registry does. The tests seed assets by
// symbol and the ledger stores ids, so one of the two has to be looked up.
func assetIDOf(t *testing.T, symbol string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`SELECT id FROM assets WHERE canonical_symbol = $1`, symbol).Scan(&id))
	return id
}
