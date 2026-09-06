//go:build integration

package worker_test

import (
	"context"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// giveCredential marks an integration runnable the way the connection flow does, by writing
// the three credential columns. The bytes are not a real credential and are never read here;
// what is under test is the index the trigger keeps.
func giveCredential(t *testing.T, accountID, integrationID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE integrations
			    SET credential_ciphertext = '\x00', wrapped_dek = '\x00', key_version = 1
			  WHERE id = $1`, integrationID)
		return err
	}))
}

// The worker's one cross-account read. It cannot go through the integrations table: FORCE
// ROW LEVEL SECURITY binds the owner too, so even a SECURITY DEFINER function over it would
// see nothing. The index in 00015 is what answers the question, and the trigger is what
// keeps it from drifting from the table it indexes.
func TestTheWorkerSeesEveryRunnableIntegrationAcrossAccounts(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)

	accountA, integrationA := seedIntegration(t)
	accountB, integrationB := seedIntegration(t)
	_, integrationWithoutCredential := seedIntegration(t)

	giveCredential(t, accountA, integrationA)
	giveCredential(t, accountB, integrationB)

	found, err := worker.ActiveIntegrations(ctx, pool)
	require.NoError(t, err)

	byIntegration := map[uuid.UUID]uuid.UUID{}
	for _, row := range found {
		byIntegration[row.IntegrationID] = row.AccountID
	}
	require.Equal(t, accountA, byIntegration[integrationA])
	require.Equal(t, accountB, byIntegration[integrationB],
		"a worker must see integrations from every account, not only one")
	require.NotContains(t, byIntegration, integrationWithoutCredential,
		"an integration with no credential cannot be ingested from, and claiming its lease"+
			" would keep another worker from a job neither of them can do")
}

// A paused or revoked integration stops being runnable the moment its status changes. The
// index is maintained by trigger precisely so that no writer has to remember this.
func TestPausingAnIntegrationTakesItOutOfTheWorkersList(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	giveCredential(t, accountID, integrationID)

	require.True(t, listed(t, integrationID))

	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE integrations SET status = 'paused' WHERE id = $1`,
			integrationID)
		return err
	}))
	require.False(t, listed(t, integrationID),
		"the index must follow the table it indexes without anyone remembering to update it")
}

// The index is protected by privilege, not by a policy: it holds no account context and
// answers a question asked before any tenant is known, exactly as account_credentials does
// for login (00003).
func TestTheAppRoleCannotReadTheIndexDirectly(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	giveCredential(t, accountID, integrationID)

	var count int
	err := appPool(t).QueryRow(ctx, `SELECT count(*) FROM worker_integrations`).Scan(&count)
	require.Error(t, err, "the app role must reach this table only through the function")
	require.Contains(t, err.Error(), "permission denied")
}

func listed(t *testing.T, integrationID uuid.UUID) bool {
	t.Helper()
	rows, err := worker.ActiveIntegrations(context.Background(), appPool(t))
	require.NoError(t, err)
	for _, row := range rows {
		if row.IntegrationID == integrationID {
			return true
		}
	}
	return false
}
