package crypto_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/stretchr/testify/require"
)

// fakeProvider answers from a fixed table, so a test can seal under one version and try to
// open under another without touching the environment.
type fakeProvider struct {
	current int
	keys    map[int][]byte
}

func (p fakeProvider) CurrentVersion() int { return p.current }

func (p fakeProvider) MasterKey(_ context.Context, version int) ([]byte, error) {
	key, ok := p.keys[version]
	if !ok {
		return nil, crypto.ErrUnknownKeyVersion
	}
	return key, nil
}

const masterV1 = "KEK-version-one-32-bytes-exactly"

func twoVersions() fakeProvider {
	return fakeProvider{current: 1, keys: map[int][]byte{
		1: []byte(masterV1),
		2: []byte("KEK-version-two-32-bytes-exactly"),
	}}
}

func TestSealOpenRoundTrip(t *testing.T) {
	ctx := context.Background()
	kp := twoVersions()
	plaintext := []byte(`{"api_key":"abc","api_secret":"def"}`)

	ciphertext, wrapped, version, err := crypto.Seal(ctx, kp, plaintext)
	require.NoError(t, err)
	require.Equal(t, 1, version, "a fresh seal uses the provider's current version")
	require.NotContains(t, string(ciphertext), "api_secret")

	got, err := crypto.Open(ctx, kp, ciphertext, wrapped, version)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

// A fresh DEK and a fresh nonce per seal. Identical ciphertext for identical plaintext
// would tell a reader of the table which two integrations hold the same credential.
func TestTwoSealsOfTheSamePlaintextDiffer(t *testing.T) {
	ctx := context.Background()
	kp := twoVersions()
	plaintext := []byte("same-secret")

	c1, w1, _, err := crypto.Seal(ctx, kp, plaintext)
	require.NoError(t, err)
	c2, w2, _, err := crypto.Seal(ctx, kp, plaintext)
	require.NoError(t, err)

	require.NotEqual(t, c1, c2, "ciphertext repeated: nonce or DEK is being reused")
	require.NotEqual(t, w1, w2, "wrapped DEK repeated: the DEK is not per-seal")
}

// GCM is authenticated. Assert it rather than trusting it: a tampered row must produce an
// error, never a shorter or garbled plaintext the caller might hand to the exchange.
func TestTamperedCiphertextFailsToOpen(t *testing.T) {
	ctx := context.Background()
	kp := twoVersions()

	ciphertext, wrapped, version, err := crypto.Seal(ctx, kp, []byte("secret-value"))
	require.NoError(t, err)

	for i := range ciphertext {
		flipped := append([]byte(nil), ciphertext...)
		flipped[i] ^= 0x01
		got, err := crypto.Open(ctx, kp, flipped, wrapped, version)
		require.Error(t, err, "byte %d flipped and Open still succeeded", i)
		require.Nil(t, got)
	}
}

func TestTamperedWrappedDEKFailsToOpen(t *testing.T) {
	ctx := context.Background()
	kp := twoVersions()

	ciphertext, wrapped, version, err := crypto.Seal(ctx, kp, []byte("secret-value"))
	require.NoError(t, err)

	for i := range wrapped {
		flipped := append([]byte(nil), wrapped...)
		flipped[i] ^= 0x01
		_, err = crypto.Open(ctx, kp, ciphertext, flipped, version)
		require.Error(t, err, "byte %d of the wrapped DEK flipped and Open still succeeded", i)
	}
}

// The DEK is what binds a ciphertext to its own row. Pairing one row's wrapped DEK with
// another row's ciphertext must fail, which is what stops a swapped column from silently
// handing one account's credential to another.
func TestWrappedDEKDoesNotOpenAnotherCiphertext(t *testing.T) {
	ctx := context.Background()
	kp := twoVersions()

	cA, wA, version, err := crypto.Seal(ctx, kp, []byte("account-A-secret"))
	require.NoError(t, err)
	cB, wB, _, err := crypto.Seal(ctx, kp, []byte("account-B-secret"))
	require.NoError(t, err)

	_, err = crypto.Open(ctx, kp, cB, wA, version)
	require.Error(t, err)
	_, err = crypto.Open(ctx, kp, cA, wB, version)
	require.Error(t, err)
}

// Rotation is additive: the version travels on the row. Opening with the wrong version
// must fail rather than fall back to the current key, because a silent fallback would make
// a rotation look complete while half the rows were still readable only by luck.
func TestOpenWithTheWrongVersionFails(t *testing.T) {
	ctx := context.Background()
	kp := twoVersions()

	ciphertext, wrapped, version, err := crypto.Seal(ctx, kp, []byte("secret-value"))
	require.NoError(t, err)
	require.Equal(t, 1, version)

	_, err = crypto.Open(ctx, kp, ciphertext, wrapped, 2)
	require.Error(t, err, "version 2's key must not open a version 1 row")

	_, err = crypto.Open(ctx, kp, ciphertext, wrapped, 99)
	require.ErrorIs(t, err, crypto.ErrUnknownKeyVersion)
}

// L13: this is the message an operator reads at 2am. It must carry enough to act on and
// nothing that would make reading it a disclosure.
func TestErrorsCarryNoKeyMaterial(t *testing.T) {
	ctx := context.Background()
	master := []byte(masterV1)
	kp := twoVersions()
	plaintext := []byte("BINANCE-REAL-SECRET-abc123")

	ciphertext, wrapped, version, err := crypto.Seal(ctx, kp, plaintext)
	require.NoError(t, err)

	tampered := append([]byte(nil), ciphertext...)
	tampered[0] ^= 0xff

	var errs []error
	_, err = crypto.Open(ctx, kp, tampered, wrapped, version)
	errs = append(errs, err)
	_, err = crypto.Open(ctx, kp, ciphertext, wrapped, 99)
	errs = append(errs, err)
	_, err = crypto.Open(ctx, kp, nil, nil, version)
	errs = append(errs, err)
	_, _, _, err = crypto.Seal(ctx, fakeProvider{current: 7}, plaintext)
	errs = append(errs, err)

	forbidden := []string{
		string(master), hex.EncodeToString(master), base64.StdEncoding.EncodeToString(master),
		string(plaintext), hex.EncodeToString(plaintext),
		hex.EncodeToString(wrapped), hex.EncodeToString(ciphertext),
	}
	for _, e := range errs {
		require.Error(t, e)
		for _, secret := range forbidden {
			require.NotContains(t, e.Error(), secret, "error leaked key material: %v", e)
		}
	}
}

// A master key of the wrong length must be refused at construction, not at the first seal:
// the process should fail to start rather than fail on the first credential.
func TestEnvFileProviderRejectsAMalformedKey(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"not base64": "this is not base64 $$$",
		"16 bytes":   base64.StdEncoding.EncodeToString([]byte("sixteen-bytes---")),
		"64 bytes":   base64.StdEncoding.EncodeToString(make([]byte, 64)),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(crypto.MasterKEKEnvVar, value)
			_, err := crypto.NewEnvFileProvider()
			require.Error(t, err)
			if value != "" {
				require.NotContains(t, err.Error(), value,
					"the provider echoed the key it rejected")
			}
			require.Contains(t, err.Error(), crypto.MasterKEKEnvVar,
				"the error must name the variable an operator has to fix")
		})
	}
}

// A rejected key must not survive into the error in any form: not whole, not as a prefix,
// not as its decoded bytes. This is the message a failing process prints where anyone can
// read it (L13).
func TestEnvFileProviderErrorNeverEchoesTheKey(t *testing.T) {
	const secret = "c3VwZXItc2VjcmV0LW1hc3Rlci1rZXktdGhhdC1pcy10b28tbG9uZy10by1iZS1hLWtlaw=="
	t.Setenv(crypto.MasterKEKEnvVar, secret)

	_, err := crypto.NewEnvFileProvider()
	require.Error(t, err, "this fixture is the wrong length on purpose")

	require.NotContains(t, err.Error(), secret)
	require.NotContains(t, err.Error(), "super-secret")
	for _, fragment := range []string{"c3VwZXI", "bWFzdGVy", "kek"} {
		require.NotContains(t, err.Error(), fragment)
	}
	require.Contains(t, err.Error(), crypto.MasterKEKEnvVar,
		"the error must still name the variable an operator has to fix")
}

func TestEnvFileProviderRoundTrips(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv(crypto.MasterKEKEnvVar, base64.StdEncoding.EncodeToString(key))

	kp, err := crypto.NewEnvFileProvider()
	require.NoError(t, err)
	require.Equal(t, 1, kp.CurrentVersion())

	ctx := context.Background()
	ciphertext, wrapped, version, err := crypto.Seal(ctx, kp, []byte("hello"))
	require.NoError(t, err)
	got, err := crypto.Open(ctx, kp, ciphertext, wrapped, version)
	require.NoError(t, err)
	require.Equal(t, []byte("hello"), got)

	_, err = kp.MasterKey(ctx, 2)
	require.ErrorIs(t, err, crypto.ErrUnknownKeyVersion)
}

// The provider must not render its key, however it is formatted (L13).
func TestEnvFileProviderDoesNotFormatItsKey(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(masterV1))
	t.Setenv(crypto.MasterKEKEnvVar, encoded)
	kp, err := crypto.NewEnvFileProvider()
	require.NoError(t, err)

	// The verbs come from a slice so this exercises the formatter rather than being
	// rewritten by a linter into a direct String call -- the accidental %v is the case
	// that matters.
	for _, verb := range []string{"%v", "%+v", "%s"} {
		rendered := fmt.Sprintf(verb, kp)
		require.NotContains(t, rendered, "KEK-version-one", "verb %s", verb)
		require.NotContains(t, rendered, encoded, "verb %s", verb)
		require.Contains(t, rendered, "REDACTED", "verb %s", verb)
	}
	require.Contains(t, kp.String(), "REDACTED")
}
