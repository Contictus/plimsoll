// Package crypto implements the envelope encryption that protects exchange credentials
// at rest (K25). A fresh data key encrypts each secret; the master key only ever encrypts
// data keys, so the master is used rarely and a rotation rewraps a short row rather than
// re-encrypting every credential.
//
// Nothing here logs, and no error carries key material (L13): the message an operator
// reads at 2am names the version and the operation, never a byte of the key.
package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// masterKeyLen is fixed at 32 bytes so both layers are AES-256. A shorter key would
// silently select AES-128 inside aes.NewCipher, which is the kind of downgrade that
// leaves no trace anywhere.
const masterKeyLen = 32

// dekLen matches masterKeyLen: the data key is itself an AES-256 key.
const dekLen = 32

// MasterKEKEnvVar is the environment variable EnvFileProvider reads. It is named here so
// that cmd/api and cmd/worker fail fast on the same name the provider actually uses.
const MasterKEKEnvVar = "PLIMSOLL_MASTER_KEK"

var (
	// ErrUnknownKeyVersion is returned when a row names a key version the provider cannot
	// supply. It is deliberately distinct from a decryption failure: one means "rotate or
	// restore the old key", the other means "this row has been tampered with".
	ErrUnknownKeyVersion = errors.New("crypto: no master key for that key version")

	// ErrDecrypt covers every failure to open a row. It is intentionally uniform — which
	// of the two layers failed, and where, is not information a caller needs and not
	// information worth handing to whoever caused the failure.
	ErrDecrypt = errors.New("crypto: could not decrypt")
)

// KeyProvider supplies master keys by version. Rotation is additive: bump CurrentVersion
// and keep answering for the old versions until every row has been rewrapped, which is
// why the version travels on the row rather than living in configuration.
//
// MasterKey takes a context because a later implementation reads from a KMS; the V1
// implementation, EnvFileProvider, does no I/O at all.
type KeyProvider interface {
	// CurrentVersion is the version a new Seal is written under.
	CurrentVersion() int
	// MasterKey returns the 32-byte master key for version, or ErrUnknownKeyVersion.
	MasterKey(ctx context.Context, version int) ([]byte, error)
}

// Seal encrypts plaintext under a freshly generated data key and returns the ciphertext,
// the data key wrapped by the current master key, and the version that wrapped it. All
// three belong on the same row: the wrapped DEK is what binds a ciphertext to its own
// row, so a ciphertext moved to another row cannot be opened by that row's DEK.
//
// Both layers are AES-256-GCM with a random nonce prefixed to the ciphertext, so a
// repeated plaintext never produces a repeated row.
func Seal(ctx context.Context, kp KeyProvider, plaintext []byte) (
	ciphertext, wrappedDEK []byte, version int, err error,
) {
	version = kp.CurrentVersion()
	master, err := masterKey(ctx, kp, version)
	if err != nil {
		return nil, nil, 0, err
	}
	defer zero(master)

	dek := make([]byte, dekLen)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, 0, fmt.Errorf("crypto: generate data key: %w", err)
	}
	defer zero(dek)

	ciphertext, err = encrypt(dek, plaintext)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("crypto: encrypt payload: %w", err)
	}
	wrappedDEK, err = encrypt(master, dek)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("crypto: wrap data key (version %d): %w", version, err)
	}
	return ciphertext, wrappedDEK, version, nil
}

// Open reverses Seal. It unwraps the data key with the master key named by version — never
// with the current key — so a row written under a retired version fails loudly instead of
// appearing to be readable by accident.
//
// Every failure below the version lookup returns ErrDecrypt. GCM authenticates, so a
// single flipped byte in either layer lands here rather than producing garbage plaintext.
func Open(ctx context.Context, kp KeyProvider, ciphertext, wrappedDEK []byte, version int) (
	[]byte, error,
) {
	master, err := masterKey(ctx, kp, version)
	if err != nil {
		return nil, err
	}
	defer zero(master)

	dek, err := decrypt(master, wrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap data key (version %d): %w", version, ErrDecrypt)
	}
	defer zero(dek)
	if len(dek) != dekLen {
		return nil, fmt.Errorf("crypto: unwrapped data key is %d bytes, want %d: %w",
			len(dek), dekLen, ErrDecrypt)
	}

	plaintext, err := decrypt(dek, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("crypto: open payload (version %d): %w", version, ErrDecrypt)
	}
	return plaintext, nil
}

// masterKey fetches and length-checks a master key, and returns a copy the caller may
// zero. The copy matters: Seal and Open wipe the buffer when they are done, and a provider
// that hands out its own storage would be disarmed by the first call otherwise.
//
// Checking the length here rather than at each call site is what stops a misconfigured
// 16-byte key from quietly selecting AES-128.
func masterKey(ctx context.Context, kp KeyProvider, version int) ([]byte, error) {
	key, err := kp.MasterKey(ctx, version)
	if err != nil {
		return nil, fmt.Errorf("crypto: master key version %d: %w", version, err)
	}
	if len(key) != masterKeyLen {
		return nil, fmt.Errorf("crypto: master key version %d is %d bytes, want %d",
			version, len(key), masterKeyLen)
	}
	return append([]byte(nil), key...), nil
}

// encrypt returns nonce || AES-256-GCM(key, nonce, plaintext). The nonce is random per
// call and carried inline, so no caller has to store or sequence one.
func encrypt(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt reverses encrypt. It returns an error for anything shorter than a nonce, so a
// truncated or absent column cannot panic its way into a stack trace.
func decrypt(key, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("sealed value is shorter than a nonce")
	}
	nonce, body := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	return gcm.Open(nil, nonce, body, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		// aes.NewCipher's own error text names the key size but never the key.
		return nil, fmt.Errorf("aes: %w", err)
	}
	return cipher.NewGCM(block)
}

// zero overwrites a key buffer once it is no longer needed. Go's garbage collector may
// still have copied it, so this narrows the window rather than closing it — which is
// worth doing and not worth claiming more for.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// EnvFileProvider reads the master key from PLIMSOLL_MASTER_KEK, base64 of exactly 32
// bytes. It is the V1 KeyProvider: one key, version 1, no rotation and no I/O. A KMS
// implementation replaces it without touching Seal or Open.
type EnvFileProvider struct {
	key []byte
}

// envFileVersion is the only version this provider answers for. When a second key is
// introduced the provider grows a map; the rows written today keep saying 1.
const envFileVersion = 1

// NewEnvFileProvider reads and validates the master key. It fails on a missing, unparsable
// or wrong-length value so that a misconfigured process refuses to start rather than
// failing on the first credential it is asked to store.
//
// The rejected value is never echoed: an operator who pasted the wrong secret into the
// variable should not find it in the log that told them so (L13).
func NewEnvFileProvider() (*EnvFileProvider, error) {
	encoded := os.Getenv(MasterKEKEnvVar)
	if encoded == "" {
		return nil, fmt.Errorf("crypto: %s is not set", MasterKEKEnvVar)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("crypto: %s is not valid base64", MasterKEKEnvVar)
	}
	if len(key) != masterKeyLen {
		return nil, fmt.Errorf("crypto: %s decodes to %d bytes, want %d",
			MasterKEKEnvVar, len(key), masterKeyLen)
	}
	return &EnvFileProvider{key: key}, nil
}

// CurrentVersion is always 1: this provider holds a single key. A second key turns this
// into a map lookup and leaves every row written today saying 1.
func (p *EnvFileProvider) CurrentVersion() int { return envFileVersion }

// MasterKey answers for version 1 only, and returns a copy so a caller that zeroes its key
// does not disarm the provider for every later call.
func (p *EnvFileProvider) MasterKey(_ context.Context, version int) ([]byte, error) {
	if version != envFileVersion {
		return nil, ErrUnknownKeyVersion
	}
	// A copy, so a caller that zeroes its key does not disarm the provider.
	return append([]byte(nil), p.key...), nil
}

// String keeps the key out of any formatted output, including the %v and %+v that reach a
// struct by accident when a caller logs its dependencies (L13).
func (p *EnvFileProvider) String() string {
	return "crypto.EnvFileProvider{key: REDACTED}"
}
