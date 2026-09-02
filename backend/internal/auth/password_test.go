package auth_test

import (
	"strings"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/stretchr/testify/require"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	encoded, err := auth.HashPassword(pw)
	require.NoError(t, err)

	ok, err := auth.VerifyPassword(encoded, pw)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = auth.VerifyPassword(encoded, pw+"!")
	require.NoError(t, err)
	require.False(t, ok)
}

// L13: the stored form must not contain the password, nor anything a reader could
// recognize as derived from it.
func TestEncodedHashNeverContainsThePassword(t *testing.T) {
	const pw = "s3cret-passphrase"
	encoded, err := auth.HashPassword(pw)
	require.NoError(t, err)
	require.NotContains(t, encoded, pw)
	require.True(t, strings.HasPrefix(encoded, "$argon2id$v=19$"),
		"expected PHC encoding, got %q", encoded)
}

// Two hashes of the same password must differ: a shared salt would let anyone who reads
// the table see which users chose the same password.
func TestHashesAreSalted(t *testing.T) {
	a, err := auth.HashPassword("same")
	require.NoError(t, err)
	b, err := auth.HashPassword("same")
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}

func TestVerifyRejectsMalformedEncoding(t *testing.T) {
	for _, bad := range []string{
		"", "not-a-hash", "$argon2id$v=19$",
		"$bcrypt$v=19$m=1,t=1,p=1$aaaa$bbbb",
		"$argon2id$v=19$m=x,t=3,p=4$aaaa$bbbb",
		"$argon2id$v=1$m=65536,t=3,p=4$aaaa$bbbb",
		"$argon2id$v=19$m=65536,t=3,p=4$!!!!$bbbb",
	} {
		_, err := auth.VerifyPassword(bad, "whatever")
		require.Error(t, err, "input %q must be rejected", bad)
	}
}

// The parameters are read back from the encoding, not assumed, so raising the cost later
// does not invalidate hashes already in the table.
func TestVerifyUsesTheParametersFromTheEncoding(t *testing.T) {
	// Produced with m=8192,t=1,p=1 -- deliberately weaker than the current defaults.
	weak, err := auth.HashPasswordWithParams("legacy-password", 1, 8192, 1)
	require.NoError(t, err)
	require.Contains(t, weak, "m=8192,t=1,p=1")

	ok, err := auth.VerifyPassword(weak, "legacy-password")
	require.NoError(t, err)
	require.True(t, ok, "a hash written with older parameters must still verify")
}
