//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/Contictus/plimsoll/backend/internal/integration"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// The two halves of a credential, distinctive enough that a grep over the raw column or a
// test log tells us immediately if either escaped.
const (
	testAPIKey    = "PLIMSOLL-TEST-APIKEY-3f9a2c"
	testAPISecret = "PLIMSOLL-TEST-SECRET-7d41be"
)

func ownerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func seedIntegration(t *testing.T) (accountID, integrationID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	accountID, integrationID = uuid.New(), uuid.New()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, email) VALUES ($1, $2)`,
			accountID, "cred-"+accountID.String()+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'test')`, integrationID, accountID)
		return err
	}))
	return accountID, integrationID
}

// keyProvider returns a deterministic provider, so a test can build a second one with a
// different master key and prove the version and the key both matter.
func keyProvider(t *testing.T, seed byte) crypto.KeyProvider {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return staticProvider{key: key}
}

type staticProvider struct{ key []byte }

func (p staticProvider) CurrentVersion() int { return 1 }

func (p staticProvider) MasterKey(_ context.Context, version int) ([]byte, error) {
	if version != 1 {
		return nil, crypto.ErrUnknownKeyVersion
	}
	return append([]byte(nil), p.key...), nil
}

func credential() integration.Credential {
	return integration.Credential{
		APIKey:    auth.Secret(testAPIKey),
		APISecret: auth.Secret(testAPISecret),
	}
}

func TestStoreAndLoadCredentialRoundTrips(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	kp := keyProvider(t, 1)
	pool := appPool(t)

	require.NoError(t, tenancy.InTx(ctx, pool, accountID, func(q *store.Queries) error {
		return integration.StoreCredential(ctx, q, kp, accountID, integrationID, credential())
	}))

	var got integration.Credential
	require.NoError(t, tenancy.InTx(ctx, pool, accountID, func(q *store.Queries) error {
		var err error
		got, err = integration.LoadCredential(ctx, q, kp, accountID, integrationID)
		return err
	}))

	require.Equal(t, testAPIKey, got.APIKey.Reveal())
	require.Equal(t, testAPISecret, got.APISecret.Reveal())
}

// At rest the row must be opaque. Rendering every byte of it as text is the check an
// auditor would run, so it is the check the test runs (K25, L13).
func TestCredentialIsUnreadableAtRest(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	kp := keyProvider(t, 1)

	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		return integration.StoreCredential(ctx, q, kp, accountID, integrationID, credential())
	}))

	var rendered string
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT encode(credential_ciphertext, 'escape')
			     || encode(wrapped_dek, 'escape')
			     || integrations::text
			   FROM integrations WHERE id = $1`, integrationID).Scan(&rendered)
	}))

	require.NotContains(t, rendered, testAPIKey)
	require.NotContains(t, rendered, testAPISecret)
	require.NotContains(t, rendered, "PLIMSOLL-TEST")
}

// The application WHERE is deliberately absent below: whatever the query returns is
// returned by RLS alone (K15, L12). A credential is the highest-value row in the schema,
// so it gets the same treatment M0 gave accounts.
func TestAnotherAccountCannotReadTheCredential(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	otherAccountID, _ := seedIntegration(t)
	kp := keyProvider(t, 1)

	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		return integration.StoreCredential(ctx, q, kp, accountID, integrationID, credential())
	}))

	var visible int
	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), otherAccountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM integrations WHERE credential_ciphertext IS NOT NULL`,
		).Scan(&visible)
	}))
	require.Zero(t, visible, "an unscoped SELECT reached another account's credential")

	err := tenancy.InTx(ctx, appPool(t), otherAccountID, func(q *store.Queries) error {
		_, err := integration.LoadCredential(ctx, q, kp, otherAccountID, integrationID)
		return err
	})
	require.ErrorIs(t, err, integration.ErrUnknownIntegration)
}

// An integration exists before its credential does, so "not stored yet" is a normal state
// and must be distinguishable from "stored and unreadable" -- the two need opposite
// responses from an operator.
func TestLoadingAnUnsetCredentialIsItsOwnError(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)

	err := tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := integration.LoadCredential(ctx, q, keyProvider(t, 1), accountID, integrationID)
		return err
	})
	require.ErrorIs(t, err, integration.ErrNoCredential)
}

func TestStoringAgainstAnotherAccountFails(t *testing.T) {
	ctx := context.Background()
	_, integrationID := seedIntegration(t)
	otherAccountID, _ := seedIntegration(t)

	err := tenancy.InTx(ctx, appPool(t), otherAccountID, func(q *store.Queries) error {
		return integration.StoreCredential(ctx, q, keyProvider(t, 1),
			otherAccountID, integrationID, credential())
	})
	require.ErrorIs(t, err, integration.ErrUnknownIntegration)
}

// A restored database with the wrong master key must fail loudly, and the failure must be
// safe to paste into a ticket.
func TestTheWrongMasterKeyFailsWithoutLeaking(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)

	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		return integration.StoreCredential(ctx, q, keyProvider(t, 1),
			accountID, integrationID, credential())
	}))

	err := tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := integration.LoadCredential(ctx, q, keyProvider(t, 99),
			accountID, integrationID)
		return err
	})
	require.Error(t, err)
	require.ErrorIs(t, err, crypto.ErrDecrypt)
	require.NotContains(t, err.Error(), testAPIKey)
	require.NotContains(t, err.Error(), testAPISecret)
	require.Contains(t, err.Error(), integrationID.String(),
		"the error must name the integration, which is what an operator needs to act")
}

// L13: a Credential reaching a log or an error by accident must render as REDACTED,
// whichever verb formats it.
func TestCredentialNeverFormatsItsSecrets(t *testing.T) {
	// Every verb someone might reach for while debugging, including the %#v that
	// auth.Secret alone cannot cover.
	for _, verb := range []string{"%v", "%+v", "%s", "%#v"} {
		rendered := fmt.Sprintf(verb, credential())
		require.NotContains(t, rendered, testAPIKey, "verb %s", verb)
		require.NotContains(t, rendered, testAPISecret, "verb %s", verb)
		require.NotContains(t, rendered, "PLIMSOLL-TEST", "verb %s", verb)
		require.Contains(t, rendered, "REDACTED", "verb %s", verb)
	}
}
