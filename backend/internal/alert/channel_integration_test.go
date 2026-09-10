//go:build integration

package alert_test

import (
	"context"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/alert"
	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// keys is the process's own provider, read from the environment the way the API and the
// worker read it -- so a channel sealed here is one they could open (K25).
func keys(t *testing.T) crypto.KeyProvider {
	t.Helper()
	kp, err := crypto.NewEnvFileProvider()
	require.NoError(t, err)
	return kp
}

// L13: the token is at rest as ciphertext, and nothing that reads the table without the key
// can send as this account. Asserted against the raw column, because "we encrypt it" is a
// claim about bytes.
func TestAStoredTokenIsNotInTheTable(t *testing.T) {
	ctx := context.Background()
	accountID := seedAccount(t)
	const token = "8199283746:AAH-THIS-IS-THE-SECRET-PART"

	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := alert.StoreTelegram(ctx, q, keys(t), accountID, "phone",
			auth.Secret(token), "12345")
		return err
	}))

	var raw []byte
	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT config_ciphertext FROM alert_channels WHERE account_id = $1`,
			accountID).Scan(&raw)
	}))
	require.NotContains(t, string(raw), token, "the bot token is stored in plaintext")
	require.NotContains(t, string(raw), "12345")
}

// A listing names channels and reveals nothing that could be used to send through them.
func TestListingChannelsRevealsNoSecret(t *testing.T) {
	ctx := context.Background()
	accountID := seedAccount(t)
	const hook = "https://hooks.example.test/T000/B111/XXXXXXXXXXXX"

	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := alert.StoreWebhook(ctx, q, keys(t), accountID, "ops", auth.Secret(hook))
		return err
	}))

	var listed []alert.Channel
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		var err error
		listed, err = alert.ListChannels(ctx, q, accountID)
		return err
	}))
	require.Len(t, listed, 1)
	require.Equal(t, "ops", listed[0].Label)
	require.NotContains(t, sprint("%#v", listed[0]), hook,
		"the listing carries the URL, which is a bearer token wearing a different hat")
}

// And the worker can decrypt them into things that send.
func TestChannelsDecryptIntoDeliverers(t *testing.T) {
	ctx := context.Background()
	accountID := seedAccount(t)
	kp := keys(t)

	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		if _, err := alert.StoreTelegram(ctx, q, kp, accountID, "phone",
			auth.Secret("token"), "12345"); err != nil {
			return err
		}
		_, err := alert.StoreWebhook(ctx, q, kp, accountID, "ops",
			auth.Secret("https://hooks.example.test/x"))
		return err
	}))

	var channels []alert.Deliverer
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		var err error
		channels, err = alert.Deliverers(ctx, q, kp, accountID, nil)
		return err
	}))
	require.Len(t, channels, 2)
}

func TestOneAccountCannotReadAnothersChannels(t *testing.T) {
	ctx := context.Background()
	owner := seedAccount(t)
	stranger := seedAccount(t)

	require.NoError(t, tenancy.InTx(ctx, appPool(t), owner, func(q *store.Queries) error {
		_, err := alert.StoreWebhook(ctx, q, keys(t), owner, "ops",
			auth.Secret("https://hooks.example.test/private"))
		return err
	}))

	var count int
	require.NoError(t, tenancy.InTxRaw(ctx, appPool(t), stranger, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_channels`).Scan(&count)
	}))
	require.Zero(t, count)
}
