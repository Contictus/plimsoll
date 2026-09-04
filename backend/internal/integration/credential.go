// Package integration owns exchange connections: the credential at rest, and the
// verification that decides whether a key is allowed to be used at all. It is the only
// package that handles a plaintext exchange secret, which is why every value that could
// carry one is an auth.Secret and every error here is written to be safe to paste into a
// ticket (K9, K25, L13).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrUnknownIntegration means no integration with that id belongs to this account.
	// It is what both a missing row and another account's row produce, because telling
	// the two apart would confirm the existence of a row the caller cannot see.
	ErrUnknownIntegration = errors.New("integration: no such integration for this account")

	// ErrNoCredential means the integration exists but has never had a credential stored.
	// Deliberately distinct from a decryption failure: this one is fixed by connecting the
	// key, that one by restoring the master key.
	ErrNoCredential = errors.New("integration: no credential stored for this integration")
)

// Credential is one exchange API key pair, in plaintext, in memory, for as long as it
// takes to use it. Both fields are auth.Secret so a careless %v or slog call prints
// REDACTED rather than the key (L13).
//
// The key is read-only by the time it gets here: Verify refuses anything else (K9).
type Credential struct {
	APIKey    auth.Secret
	APISecret auth.Secret
}

// GoString closes the one formatting verb auth.Secret cannot cover on its own. fmt
// resolves a Stringer for %v, %+v and %s even on a nested field, so the field types
// already redact those -- but %#v prints the underlying strings, and %#v is exactly what
// someone reaches for while debugging a struct. There is deliberately no String method:
// one here would redact the struct whatever its fields were typed as, and hide a later
// change from auth.Secret to a plain string (L13).
func (c Credential) GoString() string {
	return "integration.Credential{APIKey:REDACTED, APISecret:REDACTED}"
}

// wireCredential is the shape that gets encrypted. Credential itself is never marshalled:
// auth.Secret is a string underneath, so encoding/json would happily write the real value,
// and a plaintext credential must only ever be serialized on this one deliberate path.
type wireCredential struct {
	APIKey    string `json:"api_key"`
	APISecret string `json:"api_secret"`
}

// StoreCredential envelope-encrypts cred and writes it onto the integration row. q must
// come from tenancy.InTx, so the account is already bound on the transaction and RLS
// refuses a row belonging to anyone else before the UPDATE's own WHERE is consulted.
//
// It returns ErrUnknownIntegration when nothing was updated. An UPDATE that matches no row
// is otherwise silent success, which is the failure mode where a credential appears to be
// connected and every later call reports "no credential stored".
func StoreCredential(
	ctx context.Context,
	q *store.Queries,
	kp crypto.KeyProvider,
	accountID, integrationID uuid.UUID,
	cred Credential,
) error {
	plaintext, err := json.Marshal(wireCredential{
		APIKey:    cred.APIKey.Reveal(),
		APISecret: cred.APISecret.Reveal(),
	})
	if err != nil {
		return fmt.Errorf("integration: encode credential for %s: %w", integrationID, err)
	}

	ciphertext, wrapped, version, err := crypto.Seal(ctx, kp, plaintext)
	if err != nil {
		return fmt.Errorf("integration: seal credential for %s: %w", integrationID, err)
	}

	keyVersion := int32(version)
	rows, err := q.SetIntegrationCredential(ctx, store.SetIntegrationCredentialParams{
		CredentialCiphertext: ciphertext,
		WrappedDek:           wrapped,
		KeyVersion:           &keyVersion,
		AccountID:            accountID,
		IntegrationID:        integrationID,
	})
	if err != nil {
		return fmt.Errorf("integration: store credential for %s: %w", integrationID, err)
	}
	if rows == 0 {
		return fmt.Errorf("integration: store credential for %s: %w",
			integrationID, ErrUnknownIntegration)
	}
	return nil
}

// LoadCredential reads and decrypts the stored credential. The key version comes off the
// row, never from the provider's current version, so a row written under a retired key
// fails loudly instead of appearing readable by accident.
//
// Errors name the integration and nothing else: that is what an operator needs to act, and
// it is the whole of what is safe to write down.
func LoadCredential(
	ctx context.Context,
	q *store.Queries,
	kp crypto.KeyProvider,
	accountID, integrationID uuid.UUID,
) (Credential, error) {
	row, err := q.GetIntegrationCredential(ctx, store.GetIntegrationCredentialParams{
		AccountID:     accountID,
		IntegrationID: integrationID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, fmt.Errorf("integration: load credential for %s: %w",
			integrationID, ErrUnknownIntegration)
	}
	if err != nil {
		return Credential{}, fmt.Errorf("integration: load credential for %s: %w",
			integrationID, err)
	}
	// The three columns are constrained to travel together, so testing one is enough --
	// and testing key_version is the one that would also catch a hand-written row.
	if row.KeyVersion == nil {
		return Credential{}, fmt.Errorf("integration: load credential for %s: %w",
			integrationID, ErrNoCredential)
	}

	plaintext, err := crypto.Open(ctx, kp,
		row.CredentialCiphertext, row.WrappedDek, int(*row.KeyVersion))
	if err != nil {
		return Credential{}, fmt.Errorf("integration: open credential for %s: %w",
			integrationID, err)
	}

	var wire wireCredential
	if err := json.Unmarshal(plaintext, &wire); err != nil {
		// The message must not carry plaintext: a decode failure here means the bytes are
		// not what we wrote, and printing them would print a credential.
		return Credential{}, fmt.Errorf(
			"integration: decode credential for %s: stored payload is not the expected shape",
			integrationID)
	}
	return Credential{
		APIKey:    auth.Secret(wire.APIKey),
		APISecret: auth.Secret(wire.APISecret),
	}, nil
}
