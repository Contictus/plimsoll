//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// seedLeg appends one balance event and returns the seq a link names it by.
func seedLeg(
	t *testing.T, accountID, integrationID uuid.UUID, assetID int64,
	kind, amount string, at time.Time,
) int64 {
	t.Helper()
	ctx := context.Background()
	var seq int64
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO ledger_events
			   (account_id, integration_id, venue_event_id, venue_sequence, source,
			    event_type, asset_id, quantity, event_time, raw)
			 VALUES ($1, $2, $3, 0, 'rest', $4, $5, $6, $7, '{}')
			 RETURNING seq`,
			accountID, integrationID, kind+":"+uuid.NewString(), kind,
			assetID, decimal.RequireFromString(amount), at).Scan(&seq)
	}))
	return seq
}

func seedAsset(t *testing.T) int64 {
	t.Helper()
	var id int64
	require.NoError(t, ownerPool(t).QueryRow(context.Background(),
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"TL"+uuid.NewString()[:8]).Scan(&id))
	return id
}

func linkBody(outSeq, inSeq int64) string {
	return fmt.Sprintf(`{"out_seq":%d,"in_seq":%d}`, outSeq, inSeq)
}

// A user settles a case the matcher refused, and the join sticks.
//
// The matcher refuses ambiguity rather than guessing (K57), which is exactly what leaves a
// queue for a human. So this endpoint is not a convenience -- it is the other half of that
// decision.
func TestAUserCanJoinTwoLegsTheMatcherWouldNot(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("xfer-manual"))
	accountID := accountOf(t, srv, cookie)
	binanceID, bybitID := seedIntegrationFor(t, accountID), seedIntegrationFor(t, accountID)
	assetID := seedAsset(t)
	at := time.Now().UTC()

	outSeq := seedLeg(t, accountID, binanceID, assetID, "WITHDRAWAL", "1.0", at)
	inSeq := seedLeg(t, accountID, bybitID, assetID, "DEPOSIT", "0.999", at.Add(time.Hour))

	resp := send(t, http.MethodPost, srv.URL+"/transfers", linkBody(outSeq, inSeq), cookie)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/transfers", cookie))
	transfers := body["transfers"].([]any)
	require.Len(t, transfers, 1)
	one := transfers[0].(map[string]any)
	require.Equal(t, "manual", one["method"])
	require.Equal(t, "1", one["out_quantity"])
	require.Equal(t, "0.999", one["in_quantity"],
		"both halves, so the fee is visible as the difference rather than asserted")
}

// Joining two deposits is a movement that did not happen.
//
// Without the direction check the endpoint would record coins leaving a venue they arrived at,
// and the link would then be used to argue that a disposal was not one.
func TestTwoDepositsCannotBeJoined(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("xfer-twodeposits"))
	accountID := accountOf(t, srv, cookie)
	binanceID, bybitID := seedIntegrationFor(t, accountID), seedIntegrationFor(t, accountID)
	assetID := seedAsset(t)
	at := time.Now().UTC()

	first := seedLeg(t, accountID, binanceID, assetID, "DEPOSIT", "1.0", at)
	second := seedLeg(t, accountID, bybitID, assetID, "DEPOSIT", "1.0", at.Add(time.Hour))

	resp := send(t, http.MethodPost, srv.URL+"/transfers", linkBody(first, second), cookie)
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"the first leg is not a withdrawal, and saying which check failed would leak the row")
}

// Another account's ledger row is 404, not 403. Confirming that a seq exists but is not yours
// identifies another account's ledger by its row number (L12).
func TestALegBelongingToAnotherAccountIsNotFound(t *testing.T) {
	srv := newServer(t)
	mine := register(t, srv, uniqueEmail("xfer-mine"))
	theirs := register(t, srv, uniqueEmail("xfer-theirs"))

	theirAccount := accountOf(t, srv, theirs)
	theirIntegration := seedIntegrationFor(t, theirAccount)
	assetID := seedAsset(t)
	at := time.Now().UTC()
	theirOut := seedLeg(t, theirAccount, theirIntegration, assetID, "WITHDRAWAL", "1.0", at)
	theirIn := seedLeg(t, theirAccount, theirIntegration, assetID, "DEPOSIT", "1.0", at.Add(time.Hour))

	resp := send(t, http.MethodPost, srv.URL+"/transfers", linkBody(theirOut, theirIn), mine)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// A leg already half of a transfer refuses a second claim: 409 rather than 500, because the
// request was understood and the state refuses it.
func TestALegAlreadyLinkedRefusesASecondClaim(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("xfer-conflict"))
	accountID := accountOf(t, srv, cookie)
	binanceID, bybitID := seedIntegrationFor(t, accountID), seedIntegrationFor(t, accountID)
	assetID := seedAsset(t)
	at := time.Now().UTC()

	outSeq := seedLeg(t, accountID, binanceID, assetID, "WITHDRAWAL", "1.0", at)
	inSeq := seedLeg(t, accountID, bybitID, assetID, "DEPOSIT", "0.999", at.Add(time.Hour))
	other := seedLeg(t, accountID, bybitID, assetID, "DEPOSIT", "0.999", at.Add(2*time.Hour))

	require.Equal(t, http.StatusCreated,
		send(t, http.MethodPost, srv.URL+"/transfers", linkBody(outSeq, inSeq), cookie).StatusCode)

	resp := send(t, http.MethodPost, srv.URL+"/transfers", linkBody(outSeq, other), cookie)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

// A join can be undone, and undoing it touches no event.
func TestAJoinCanBeUndoneWithoutTouchingTheLedger(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("xfer-undo"))
	accountID := accountOf(t, srv, cookie)
	binanceID, bybitID := seedIntegrationFor(t, accountID), seedIntegrationFor(t, accountID)
	assetID := seedAsset(t)
	at := time.Now().UTC()

	outSeq := seedLeg(t, accountID, binanceID, assetID, "WITHDRAWAL", "1.0", at)
	inSeq := seedLeg(t, accountID, bybitID, assetID, "DEPOSIT", "0.999", at.Add(time.Hour))
	send(t, http.MethodPost, srv.URL+"/transfers", linkBody(outSeq, inSeq), cookie)

	before := ledgerFingerprint(t, accountID)

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/transfers", cookie))
	id := body["transfers"].([]any)[0].(map[string]any)["id"].(string)

	resp := send(t, http.MethodDelete, srv.URL+"/transfers/"+id, "", cookie)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	require.Empty(t, decodeJSON(t,
		do(t, http.MethodGet, srv.URL+"/transfers", cookie))["transfers"])
	require.Equal(t, before, ledgerFingerprint(t, accountID),
		"unjoining is an assertion withdrawn, not an event deleted (L2)")
}

// Undoing a transfer that is not yours, or does not exist, is the same answer.
func TestUndoingAnUnknownTransferIsNotFound(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("xfer-undo-unknown"))

	resp := send(t, http.MethodDelete, srv.URL+"/transfers/"+uuid.NewString(), "", cookie)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// Every transfer route needs a session.
func TestTransfersRequireASession(t *testing.T) {
	srv := newServer(t)
	require.Equal(t, http.StatusUnauthorized,
		do(t, http.MethodGet, srv.URL+"/transfers", nil).StatusCode)
	require.Equal(t, http.StatusUnauthorized,
		send(t, http.MethodPost, srv.URL+"/transfers", linkBody(1, 2), nil).StatusCode)
}
