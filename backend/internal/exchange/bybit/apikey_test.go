package bybit_test

import (
	"os"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/exchange/bybit"
	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/fixtures/bybit/" + name)
	require.NoError(t, err)
	return raw
}

// The key K9 is built around: read-only, and holding nothing but history.
func TestAReadOnlyKeyIsAccepted(t *testing.T) {
	info, err := bybit.ParsePermissions(fixture(t, "apikey_read_only.json"))
	require.NoError(t, err)
	require.True(t, info.ReadOnly)
	require.Equal(t, []string{"ExchangeHistory"}, info.Granted)
}

// A key that can move money off the exchange never reaches the rest of the system, so no
// later code has to be careful with it (K9, L13).
func TestAKeyThatCanWithdrawIsRefused(t *testing.T) {
	_, err := bybit.ParsePermissions(fixture(t, "apikey_withdraw.json"))
	require.ErrorIs(t, err, bybit.ErrOverPermissioned)
	require.Contains(t, err.Error(), "Withdraw", "the message says what to turn off")
}

// readOnly=0 with an empty permissions object is the case that makes the two checks
// independent: the allowlist alone would have accepted this key, because it holds nothing.
// The venue says it can write, and that is enough on its own (B5).
func TestAReadWriteKeyIsRefusedEvenWithNoPermissionListed(t *testing.T) {
	_, err := bybit.ParsePermissions(fixture(t, "apikey_read_write.json"))
	require.ErrorIs(t, err, bybit.ErrNotReadOnly)
}

// The allowlist is closed. A venue that adds a capability must not have it granted silently
// by a denylist that has never heard of it.
func TestAPermissionWeHaveNeverHeardOfIsRefused(t *testing.T) {
	_, err := bybit.ParsePermissions(fixture(t, "apikey_unknown_permission.json"))
	require.ErrorIs(t, err, bybit.ErrOverPermissioned)
	require.Contains(t, err.Error(), "DoAnything")
}

// A payload missing the field the decision rests on is a payload we cannot decide from.
// Defaulting the absence to "read-only" would accept every key whose shape we failed to parse
// -- which is the same as having no check at all, on exactly the day the venue changes it.
func TestAMissingReadOnlyFieldIsRefusedRatherThanAssumed(t *testing.T) {
	_, err := bybit.ParsePermissions([]byte(`{"permissions":{"Exchange":["ExchangeHistory"]}}`))
	require.ErrorIs(t, err, bybit.ErrMalformedKeyInfo)
}

// Malformed input is an error, never a permissive default.
func TestMalformedKeyInfoIsRefused(t *testing.T) {
	for _, bad := range []string{``, `not json`, `[]`} {
		_, err := bybit.ParsePermissions([]byte(bad))
		require.Error(t, err, "input %q", bad)
	}
}

// An error about a key never contains the key. The caller already knows which one it handed
// over, and the message is written to be safe to show a user (L13).
func TestNoRefusalCarriesTheCredential(t *testing.T) {
	_, err := bybit.ParsePermissions(fixture(t, "apikey_withdraw.json"))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "REDACTED",
		"not even the placeholder: nothing from the key's own fields belongs in an error")
}
