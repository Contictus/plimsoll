//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func recordFinding(
	t *testing.T, accountID, integrationID uuid.UUID, f quality.Finding, at time.Time,
) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		return quality.Record(ctx, q, accountID, integrationID, quality.Pass{
			At:    at,
			Owns:  quality.ReconciliationKinds,
			Found: []quality.Finding{f},
		})
	}))
}

func aMismatch() quality.Finding {
	return quality.Finding{
		Kind:     quality.KindMissingEvent,
		Subject:  "BTC",
		Severity: quality.SeverityError,
		Detail:   "the ledger and the venue disagree",
		Delta:    decimal.NewNullDecimal(decimal.RequireFromString("0.5")),
	}
}

// An account with nothing wrong gets 200 and an empty list -- not an error.
//
// An error status would make "we are fine" indistinguishable from "we could not tell you",
// which is the one distinction this entire endpoint exists to preserve.
func TestACleanRegisterIsAnEmptyListAndNotAnError(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("dq-clean"))

	resp := do(t, http.MethodGet, srv.URL+"/data-quality", cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := decodeJSON(t, resp)
	require.Empty(t, body["open"])
	require.Equal(t, "ok", body["freshness"].(map[string]any)["status"])
}

// An open finding makes the response say so. reconciliation_mismatch has existed since M0
// with no producer; this asserts the endpoint is one.
func TestAnOpenFindingDegradesTheResponse(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("dq-open"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)

	recordFinding(t, accountID, integrationID, aMismatch(), time.Now().UTC())

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/data-quality", cookie))
	open := body["open"].([]any)
	require.Len(t, open, 1)
	require.Equal(t, "missing_event", open[0].(map[string]any)["kind"])
	require.Equal(t, "0.5", open[0].(map[string]any)["delta"],
		"the delta crosses as a string like every other number (L1)")

	fresh := body["freshness"].(map[string]any)
	require.Equal(t, "unreliable", fresh["status"])
	codes := []string{}
	for _, r := range fresh["reasons"].([]any) {
		codes = append(codes, r.(map[string]any)["code"].(string))
	}
	require.Contains(t, codes, "reconciliation_mismatch")
}

// A finding with no magnitude renders as EMPTY, never as "0". A problem we could not measure
// and a problem measured at zero are opposite claims, and rendering both as zero is how the
// second one gets ignored (L11).
func TestAFindingWithNoMagnitudeIsEmptyAndNotZero(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("dq-null"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)

	recordFinding(t, accountID, integrationID, quality.Finding{
		Kind:     quality.KindSnapshotFailed,
		Subject:  "spot",
		Severity: quality.SeverityWarn,
		Detail:   "the venue could not be asked",
	}, time.Now().UTC())

	body := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/data-quality", cookie))
	require.Equal(t, "", body["open"].([]any)[0].(map[string]any)["delta"])
}

// The front page is the present tense. A closed finding appears only when asked for, or a
// resolved problem reads as a live one.
func TestAClosedFindingIsOutOfTheWayUntilAskedFor(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("dq-history"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	at := time.Now().UTC()

	recordFinding(t, accountID, integrationID, aMismatch(), at)
	// The next pass sees nothing, which closes it.
	require.NoError(t, tenancy.InTx(context.Background(), appPool(t), accountID,
		func(q *store.Queries) error {
			return quality.Record(context.Background(), q, accountID, integrationID,
				quality.Pass{At: at.Add(time.Minute), Owns: quality.ReconciliationKinds})
		}))

	require.Empty(t, decodeJSON(t, do(t, http.MethodGet, srv.URL+"/data-quality", cookie))["open"])
	withHistory := decodeJSON(t,
		do(t, http.MethodGet, srv.URL+"/data-quality?history=true", cookie))
	require.Len(t, withHistory["open"].([]any), 1)
	require.NotNil(t, withHistory["open"].([]any)[0].(map[string]any)["closed_at"])
}

// One account's register is one account's.
func TestTheRegisterIsNotSharedBetweenAccounts(t *testing.T) {
	srv := newServer(t)
	mine := register(t, srv, uniqueEmail("dq-mine"))
	theirs := register(t, srv, uniqueEmail("dq-theirs"))
	accountID := accountOf(t, srv, mine)

	recordFinding(t, accountID, seedIntegrationFor(t, accountID), aMismatch(), time.Now().UTC())

	require.Len(t, decodeJSON(t, do(t, http.MethodGet, srv.URL+"/data-quality", mine))["open"], 1)
	require.Empty(t, decodeJSON(t, do(t, http.MethodGet, srv.URL+"/data-quality", theirs))["open"])
}

// seedScope gives an integration a finished walk, so a resync has something to rewind.
func seedScope(t *testing.T, accountID, integrationID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO backfill_progress
			   (account_id, integration_id, scope, cursor, completed_at)
			 VALUES ($1, $2, 'trades:BTCUSDT', '12345', now())`,
			accountID, integrationID)
		return err
	}))
}

// K55, and the test the whole policy rests on.
//
// Resync rewinds the walk and NOTHING else. It writes no correction event and leaves every
// existing ledger row exactly as it was (L2) -- auto-correcting a misclassified finding writes
// a wrong correction into an append-only ledger, and that cannot be undone.
func TestResyncReopensTheWalkAndWritesNoCorrection(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("resync"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	seedScope(t, accountID, integrationID)

	before := ledgerFingerprint(t, accountID)

	resp := send(t, http.MethodPost,
		srv.URL+"/integrations/"+integrationID.String()+"/resync", "", cookie)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.EqualValues(t, 1, decodeJSON(t, resp)["scopes_reopened"])

	var cursor string
	var completed *time.Time
	ctx := context.Background()
	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT cursor, completed_at FROM backfill_progress WHERE integration_id = $1`,
			integrationID).Scan(&cursor, &completed)
	}))
	require.Equal(t, "", cursor,
		"the cursor is rewound to 'nothing walked yet', which the schema spells as empty")
	require.Nil(t, completed, "a finished walk is unfinished again")

	require.Equal(t, before, ledgerFingerprint(t, accountID),
		"resync must not have written anything into the ledger")
}

// ledgerFingerprint is the ledger's whole observable state for one account: if resync touched
// a row, added one, or removed one, this changes.
func ledgerFingerprint(t *testing.T, accountID uuid.UUID) string {
	t.Helper()
	ctx := context.Background()
	var fingerprint *string
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT md5(string_agg(venue_event_id || ':' || event_type, ',' ORDER BY seq))
			   FROM ledger_events WHERE account_id = $1`, accountID).Scan(&fingerprint)
	}))
	if fingerprint == nil {
		return "empty"
	}
	return *fingerprint
}

// Another account's integration is 404, not 403.
//
// Telling a caller that an id exists but is not theirs confirms another account's data by its
// identifier -- the leak is the status code itself (L12).
func TestResyncRefusesAnotherAccountsIntegrationAsNotFound(t *testing.T) {
	srv := newServer(t)
	mine := register(t, srv, uniqueEmail("resync-mine"))
	theirs := register(t, srv, uniqueEmail("resync-theirs"))
	theirIntegration := seedIntegrationFor(t, accountOf(t, srv, theirs))

	resp := send(t, http.MethodPost,
		srv.URL+"/integrations/"+theirIntegration.String()+"/resync", "", mine)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// An integration that does not exist at all answers exactly the same way, or the difference
// between the two answers is itself the oracle.
func TestResyncOnAnUnknownIntegrationIsAlsoNotFound(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("resync-unknown"))

	resp := send(t, http.MethodPost,
		srv.URL+"/integrations/"+uuid.NewString()+"/resync", "", cookie)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// Resync requires a session like everything else that touches an account.
func TestResyncRequiresASession(t *testing.T) {
	srv := newServer(t)
	resp := send(t, http.MethodPost,
		srv.URL+"/integrations/"+uuid.NewString()+"/resync", "", nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// The whole milestone, reaching the surface it was built for.
//
// A disagreement the register knows about must change what /portfolio says about itself. A
// portfolio that reports "ok" while an open finding contradicts it is confident and wrong,
// which L11 exists to forbid -- and reconciliation_mismatch, declared in M0, finally has a
// path from a finding to a response.
func TestAnOpenFindingReachesThePortfolioAsAFreshnessReason(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("dq-portfolio"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)

	before := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))
	require.NotContains(t, codesIn(before), "reconciliation_mismatch",
		"nothing is wrong yet, so nothing may be claimed")

	recordFinding(t, accountID, integrationID, aMismatch(), time.Now().UTC())

	after := decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))
	require.Contains(t, codesIn(after), "reconciliation_mismatch")

	// The reason's OWN severity, not the response's status. A response can be unreliable for
	// half a dozen unrelated reasons, so asserting the status would pass whatever this
	// particular reason claimed -- which is how a gap we cannot account for quietly becomes
	// a warning.
	require.Equal(t, "error", severityOf(t, after, "reconciliation_mismatch"),
		"a delta nothing explains is not a warning: the numbers are unverified")
}

// A finding that has been closed says nothing about the present, so it must not degrade a
// response. Reporting a fixed problem is how a user learns that the banner means nothing.
func TestAClosedFindingDoesNotDegradeThePortfolio(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("dq-closed"))
	accountID := accountOf(t, srv, cookie)
	integrationID := seedIntegrationFor(t, accountID)
	at := time.Now().UTC()

	recordFinding(t, accountID, integrationID, aMismatch(), at)
	require.Contains(t,
		codesIn(decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))),
		"reconciliation_mismatch")

	// The next pass sees nothing, which closes it.
	require.NoError(t, tenancy.InTx(context.Background(), appPool(t), accountID,
		func(q *store.Queries) error {
			return quality.Record(context.Background(), q, accountID, integrationID,
				quality.Pass{At: at.Add(time.Minute), Owns: quality.ReconciliationKinds})
		}))

	require.NotContains(t,
		codesIn(decodeJSON(t, do(t, http.MethodGet, srv.URL+"/portfolio", cookie))),
		"reconciliation_mismatch",
		"the problem is over, and the response must stop saying otherwise")
}

func severityOf(t *testing.T, body map[string]any, code string) string {
	t.Helper()
	for _, r := range body["freshness"].(map[string]any)["reasons"].([]any) {
		reason := r.(map[string]any)
		if reason["code"] == code {
			return reason["severity"].(string)
		}
	}
	t.Fatalf("no reason %q in %v", code, codesIn(body))
	return ""
}

func codesIn(body map[string]any) []string {
	out := []string{}
	fresh, ok := body["freshness"].(map[string]any)
	if !ok {
		return out
	}
	for _, r := range fresh["reasons"].([]any) {
		out = append(out, r.(map[string]any)["code"].(string))
	}
	return out
}
