package auth_test

import (
	"log/slog"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestOpaqueTokenIsUnpredictableAndHashesConsistently(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		plain, hash, err := auth.NewOpaqueToken()
		require.NoError(t, err)
		require.False(t, seen[plain], "NewOpaqueToken returned a duplicate")
		seen[plain] = true
		require.Len(t, hash, 32, "token hash must be SHA-256")
		require.Equal(t, hash, auth.HashToken(plain))
	}
}

// A session token carries the account it belongs to. That is what lets
// auth_resolve_session bind app.account_id before it reads the sessions table, which
// otherwise cannot be read at all under FORCE ROW LEVEL SECURITY (K15, K16).
func TestSessionTokenCarriesItsAccount(t *testing.T) {
	accountID := uuid.New()
	plain, hash, err := auth.NewSessionToken(accountID)
	require.NoError(t, err)
	require.Len(t, hash, 32)

	got, err := auth.AccountFromSessionToken(plain)
	require.NoError(t, err)
	require.Equal(t, accountID, got)
}

// The account id in the token is a routing hint, never a credential. Rewriting it must
// change the hash, so the altered token matches no stored session.
func TestRewritingTheAccountChangesTheHash(t *testing.T) {
	original, hash, err := auth.NewSessionToken(uuid.New())
	require.NoError(t, err)

	_, secret, found := auth.SplitSessionToken(original)
	require.True(t, found)

	forged := auth.JoinSessionToken(uuid.New(), secret)
	require.NotEqual(t, original, forged)
	require.NotEqual(t, hash, auth.HashToken(forged),
		"a forged account prefix must not hash to the stored session")
}

func TestSessionTokensAreUniquePerIssue(t *testing.T) {
	accountID := uuid.New()
	seen := map[string]bool{}
	for range 100 {
		plain, _, err := auth.NewSessionToken(accountID)
		require.NoError(t, err)
		require.False(t, seen[plain], "the same account got a duplicate session token")
		seen[plain] = true
	}
}

func TestAccountFromSessionTokenRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "no-separator", ".", "a.b", "!!!.xyz", "."} {
		_, err := auth.AccountFromSessionToken(bad)
		require.Error(t, err, "input %q must be rejected", bad)
	}
}

// L13: a secret must be unreadable through the logger, whichever way it is passed.
func TestSecretRedactsInLogs(t *testing.T) {
	s := auth.Secret("super-secret-api-key")
	require.Equal(t, "REDACTED", s.LogValue().String())
	require.Equal(t, "REDACTED", s.String())

	// Compile-time proof that slog will route through LogValue rather than formatting the
	// underlying string.
	var _ slog.LogValuer = s
	v := s.LogValue()
	require.NotContains(t, v.String(), "super-secret")
	require.Equal(t, "super-secret-api-key", s.Reveal())
}
