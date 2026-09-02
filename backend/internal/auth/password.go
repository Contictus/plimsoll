// Package auth holds credential primitives. The hashing and token code here is pure --
// no clock, no database, no logger (L4) -- so it is unit-tested without Docker.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. Memory dominates the cost for an attacker with parallel hardware,
// which is why it is the parameter raised furthest above the OWASP floor.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// ErrMalformedHash means the stored encoding could not be parsed. It is deliberately
// distinct from a wrong password: one is a data problem, the other is a normal
// authentication outcome.
var ErrMalformedHash = errors.New("auth: malformed password hash")

// HashPassword returns a PHC-encoded argon2id hash at the current parameters:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<b64 salt>$<b64 key>
func HashPassword(password string) (string, error) {
	return HashPasswordWithParams(password, argonTime, argonMemory, argonThreads)
}

// HashPasswordWithParams is HashPassword with the cost stated explicitly. It exists so
// that raising the defaults later is a one-line change covered by a test that still
// exercises the old cost, rather than a migration over every stored hash.
func HashPasswordWithParams(password string, time, memory uint32, threads uint8) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, time, memory, threads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, time, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches encoded. It returns an error only when
// encoded is unparsable; a wrong password is (false, nil).
//
// The cost parameters are read back from the encoding rather than assumed, so hashes
// written under older settings keep verifying after the defaults are raised.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, ErrMalformedHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrMalformedHash
	}

	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, ErrMalformedHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, ErrMalformedHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, ErrMalformedHash
	}

	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyVerify performs one argon2id derivation and discards the result. Login calls it
// when no account matches, so a missing email and a wrong password cost the same time and
// the endpoint cannot be used to enumerate users.
func DummyVerify() {
	_ = argon2.IDKey([]byte("dummy"), make([]byte, argonSaltLen),
		argonTime, argonMemory, argonThreads, argonKeyLen)
}
