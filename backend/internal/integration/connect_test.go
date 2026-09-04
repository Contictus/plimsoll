package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/integration"
	"github.com/stretchr/testify/require"
)

func fixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "binance", name))
	require.NoError(t, err)
	return raw
}

// unitCredential is separate from the integration test's credential(): this file is
// compiled without the integration build tag, so it cannot share that helper.
func unitCredential() integration.Credential {
	return integration.Credential{
		APIKey:    auth.Secret("UNIT-APIKEY-a1b2c3"),
		APISecret: auth.Secret("UNIT-SECRET-d4e5f6"),
	}
}

type fakeReader struct {
	raw  json.RawMessage
	err  error
	seen integration.Credential
}

func (r *fakeReader) APIRestrictions(_ context.Context, cred integration.Credential) (
	json.RawMessage, error,
) {
	r.seen = cred
	return r.raw, r.err
}

func TestReadOnlyKeyVerifies(t *testing.T) {
	perms, err := integration.ParsePermissions(fixture(t, "api_restrictions_read_only.json"))
	require.NoError(t, err)

	require.True(t, perms.Reading)
	require.True(t, perms.FixReadOnly)
	require.True(t, perms.IPRestricted, "ipRestrict is a restriction, not a capability")

	require.False(t, perms.Withdrawals)
	require.False(t, perms.SpotAndMarginTrading)
	require.False(t, perms.Margin)
	require.False(t, perms.Futures)
	require.False(t, perms.InternalTransfer)
	require.False(t, perms.UniversalTransfer)
	require.False(t, perms.VanillaOptions)
	require.False(t, perms.FixAPITrade)
	require.False(t, perms.PortfolioMarginTrading)
}

// K9: rejected, not accepted with a warning. Each case names the permission the user has
// to turn off -- an error that says "rejected" without saying why makes them disable the
// wrong thing and try again.
func TestOverPermissionedKeysAreRejectedAndNameThePermission(t *testing.T) {
	tests := map[string]struct {
		file string
		name string
	}{
		"withdrawals": {"api_restrictions_withdrawals.json", "enableWithdrawals"},
		"spot trading": {
			"api_restrictions_spot_trading.json", "enableSpotAndMarginTrading",
		},
		"an unknown permission": {
			"api_restrictions_unknown_permission.json", "enableQuantumSettlement",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := integration.ParsePermissions(fixture(t, tc.file))
			require.ErrorIs(t, err, integration.ErrOverPermissioned)
			require.Contains(t, err.Error(), tc.name)
		})
	}
}

// The vendor's own response example is over-permissioned. Rejecting it is not a quirk of
// the fixture -- it is the check working on the most realistic input available.
func TestTheDocumentedExampleIsRejected(t *testing.T) {
	_, err := integration.ParsePermissions(
		fixture(t, "api_restrictions_documented_example.json"))
	require.ErrorIs(t, err, integration.ErrOverPermissioned)

	// All three offending permissions are named, not just the first one found: a user who
	// fixes one and retries should not discover the next one on the next attempt.
	for _, name := range []string{
		"enableInternalTransfer", "permitsUniversalTransfer", "enablePortfolioMarginTrading",
	} {
		require.Contains(t, err.Error(), name)
	}
}

// A field with no capability behind it must not trip the unknown-permission rule.
// Otherwise every key stops verifying the day Binance adds a timestamp, and the fix under
// pressure is to weaken the rule that stops a trading key.
func TestANewNonBooleanFieldIsNotAPermission(t *testing.T) {
	perms, err := integration.ParsePermissions(
		fixture(t, "api_restrictions_new_field_not_a_permission.json"))
	require.NoError(t, err)
	require.True(t, perms.Reading)
}

// A key that cannot read is a different failure with a different fix, so it gets a
// different error. Folding it into ErrOverPermissioned would tell the user to remove a
// permission when what they need is to add one.
func TestAKeyThatCannotReadIsRejectedSeparately(t *testing.T) {
	_, err := integration.ParsePermissions(fixture(t, "api_restrictions_no_reading.json"))
	require.ErrorIs(t, err, integration.ErrNotReadable)
	require.NotErrorIs(t, err, integration.ErrOverPermissioned)
	require.Contains(t, err.Error(), "enableReading")
}

// A malformed response must say so. Reporting it as ErrNotReadable would send an operator
// to re-issue a key that was fine, while the actual fault -- an error page, a proxy, a
// truncated body -- goes uninvestigated. Asserting the specific error is what makes this
// distinction real rather than decorative.
func TestMalformedResponseIsItsOwnError(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":            "",
		"not json":         "<html>451</html>",
		"json array":       "[]",
		"json null":        "null",
		"json string":      `"unauthorized"`,
		"truncated object": `{"enableReading": tru`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := integration.ParsePermissions(json.RawMessage(raw))
			require.ErrorIs(t, err, integration.ErrMalformedRestrictions)
			require.NotErrorIs(t, err, integration.ErrNotReadable,
				"a broken response must not be reported as a key that cannot read")
			require.NotErrorIs(t, err, integration.ErrOverPermissioned)
		})
	}
}

func TestVerifyPassesTheCredentialThroughAndParses(t *testing.T) {
	reader := &fakeReader{raw: fixture(t, "api_restrictions_read_only.json")}

	perms, err := integration.Verify(context.Background(), reader, unitCredential())
	require.NoError(t, err)
	require.True(t, perms.Reading)
	require.Equal(t, "UNIT-APIKEY-a1b2c3", reader.seen.APIKey.Reveal())
}

func TestVerifyPropagatesATransportError(t *testing.T) {
	boom := errors.New("connection refused")
	reader := &fakeReader{err: boom}

	_, err := integration.Verify(context.Background(), reader, unitCredential())
	require.ErrorIs(t, err, boom)
}

// L13: the key is the one thing that must never reach the message. It is also the thing a
// naive implementation would include, to say which key was rejected.
func TestVerificationErrorsNeverCarryTheCredential(t *testing.T) {
	cred := unitCredential()

	var errs []error
	for _, file := range []string{
		"api_restrictions_withdrawals.json",
		"api_restrictions_unknown_permission.json",
		"api_restrictions_no_reading.json",
	} {
		_, err := integration.Verify(context.Background(),
			&fakeReader{raw: fixture(t, file)}, cred)
		errs = append(errs, err)
	}
	_, err := integration.Verify(context.Background(),
		&fakeReader{raw: json.RawMessage("<html>")}, cred)
	errs = append(errs, err)

	for _, e := range errs {
		require.Error(t, e)
		require.NotContains(t, e.Error(), cred.APIKey.Reveal())
		require.NotContains(t, e.Error(), cred.APISecret.Reveal())
		require.NotContains(t, e.Error(), "UNIT-")
	}
}

// Every fixture in the directory must declare where its bytes came from. A payload whose
// provenance nobody recorded is one nobody can decide to trust or replace.
func TestEveryFixtureDeclaresItsSource(t *testing.T) {
	paths, err := filepath.Glob(
		filepath.Join("..", "..", "testdata", "fixtures", "binance", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)

			var envelope struct {
				Source string `json:"_source"`
				Why    string `json:"_why"`
			}
			require.NoError(t, json.Unmarshal(raw, &envelope))
			require.Contains(t, []string{"recorded", "documented", "derived"}, envelope.Source)
			require.NotEmpty(t, envelope.Why)
		})
	}
}
