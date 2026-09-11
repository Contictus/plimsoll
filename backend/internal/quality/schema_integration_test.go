//go:build integration

package quality_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// L12: RLS enabled is not enough. Enabled alone still exempts the table owner, which is the
// trap K15 names, and this project connects its worker as a non-owner precisely so the
// backstop is real rather than decorative.
func TestFindingsHasRLSEnabledAndForced(t *testing.T) {
	ctx := context.Background()
	var enabled, forced bool
	require.NoError(t, ownerPool(t).QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'findings'::regclass`,
	).Scan(&enabled, &forced))
	require.True(t, enabled, "findings: ROW LEVEL SECURITY not enabled")
	require.True(t, forced, "findings: ROW LEVEL SECURITY not forced")
}

// The application may open, touch and close a finding. It may not make one disappear.
//
// A closed finding is the evidence its classification is argued from, and K55 makes that
// evidence the precondition for ever correcting anything automatically. A DELETE grant is how
// that evidence quietly stops existing.
func TestTheApplicationCannotDeleteAFinding(t *testing.T) {
	ctx := context.Background()
	var canDelete bool
	require.NoError(t, ownerPool(t).QueryRow(ctx,
		`SELECT has_table_privilege('plimsoll_app', 'findings', 'DELETE')`,
	).Scan(&canDelete))
	require.False(t, canDelete, "the app role must not be able to delete a finding")
}

// A finding that closes before it opens is not a finding, it is a corrupted row. The CHECK is
// cheap and the state it forbids is unreadable.
func TestAFindingCannotCloseBeforeItOpens(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedAccount(t)

	_, err := ownerPool(t).Exec(ctx,
		`INSERT INTO findings
		   (account_id, integration_id, kind, subject, severity, opened_at, last_seen_at, closed_at)
		 VALUES ($1, $2, 'missing_event', 'BTC', 'error',
		         now(), now(), now() - interval '1 hour')`,
		accountID, integrationID)
	require.Error(t, err, "closed_at before opened_at must be refused by the schema")
}
