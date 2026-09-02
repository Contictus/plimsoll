package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
)

const tokenSecretBytes = 32

// ErrMalformedToken means the value is not shaped like a session token at all. It never
// distinguishes a wrong account from a wrong secret: only the stored hash decides that.
var ErrMalformedToken = errors.New("auth: malformed session token")

// NewOpaqueToken mints a token with no structure, used for invites -- which exist before
// any account does. The plaintext is returned to the caller once and never stored; only
// its SHA-256 hash is persisted, so a database leak does not yield usable tokens.
func NewOpaqueToken() (plaintext string, hash []byte, err error) {
	raw := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: read token: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(raw)
	return plaintext, HashToken(plaintext), nil
}

// NewSessionToken mints a session token of the form <account>.<secret>.
//
// The account prefix is not a credential and grants nothing: the stored row is keyed on
// the hash of the whole string, so rewriting the prefix simply produces a token that
// matches no session. Its job is to let auth_resolve_session bind app.account_id BEFORE
// it reads the sessions table -- which, under FORCE ROW LEVEL SECURITY, is the only way
// that read can return anything at all (K15, K16).
//
// Opaque and revocable by design -- explicitly not a JWT, which cannot be revoked before
// it expires.
func NewSessionToken(accountID uuid.UUID) (plaintext string, hash []byte, err error) {
	raw := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: read token: %w", err)
	}
	plaintext = JoinSessionToken(accountID, base64.RawURLEncoding.EncodeToString(raw))
	return plaintext, HashToken(plaintext), nil
}

// JoinSessionToken assembles the wire form. It is exported so that a test can build a
// token with a deliberately wrong account prefix and confirm it is rejected.
func JoinSessionToken(accountID uuid.UUID, secret string) string {
	id := accountID
	return base64.RawURLEncoding.EncodeToString(id[:]) + "." + secret
}

// SplitSessionToken separates the account prefix from the secret. ok is false when the
// value is not shaped like a session token.
func SplitSessionToken(plaintext string) (accountID uuid.UUID, secret string, ok bool) {
	prefix, secret, found := strings.Cut(plaintext, ".")
	if !found || secret == "" {
		return uuid.Nil, "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(prefix)
	if err != nil || len(raw) != len(uuid.UUID{}) {
		return uuid.Nil, "", false
	}
	copy(accountID[:], raw)
	return accountID, secret, true
}

// AccountFromSessionToken reads the account the token claims to belong to. The claim is
// unverified: it is used only to scope the lookup that then verifies it.
func AccountFromSessionToken(plaintext string) (uuid.UUID, error) {
	accountID, _, ok := SplitSessionToken(plaintext)
	if !ok {
		return uuid.Nil, ErrMalformedToken
	}
	return accountID, nil
}

// HashToken is the one-way lookup key for a token. A plain SHA-256 is correct here,
// unlike for a password, because the input is 256 bits of entropy rather than a
// human-chosen secret.
func HashToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// Secret wraps a value that must never be printed. It satisfies slog.LogValuer and
// fmt.Stringer, so it stays redacted whether it is logged structurally or interpolated
// into a message by accident (L13).
type Secret string

func (s Secret) LogValue() slog.Value { return slog.StringValue("REDACTED") }
func (s Secret) String() string       { return "REDACTED" }

// Reveal returns the underlying value. Every call site is a place a secret can escape, so
// keep them few and obvious.
func (s Secret) Reveal() string { return string(s) }
