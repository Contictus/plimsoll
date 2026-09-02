// Package config validates process configuration at startup. It holds the checks both
// binaries need, so a security check cannot be right in one main and stale in the other.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
)

// MasterKEKBytes is the length of the envelope-encryption master key (K25). AES-256 is
// what wraps each account's DEK, so anything shorter silently weakens every credential in
// the database.
const MasterKEKBytes = 32

// CheckMasterKEK validates PLIMSOLL_MASTER_KEK without returning, storing or logging it.
// M0 only proves it is present and well-formed; the key itself is used from M2. Failing at
// startup is the point -- a process that boots and then cannot decrypt any integration is
// far harder to diagnose than one that refuses to start.
//
// No error here echoes the key or anything derived from it (L13): an operator learns the
// variable name and the shape expected, never the value they got wrong.
func CheckMasterKEK(encoded string) error {
	if encoded == "" {
		return errors.New("PLIMSOLL_MASTER_KEK is not set")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// The wrapped error would quote the offending byte of the key, so it is dropped.
		return errors.New("PLIMSOLL_MASTER_KEK is not valid base64")
	}
	if len(raw) != MasterKEKBytes {
		return fmt.Errorf("PLIMSOLL_MASTER_KEK must decode to %d bytes, got %d",
			MasterKEKBytes, len(raw))
	}
	return nil
}
