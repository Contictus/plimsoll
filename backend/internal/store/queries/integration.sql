-- name: SetIntegrationCredential :execrows
-- account_id is the application-level scope (L12); RLS is the backstop underneath it. The
-- row count is the caller's evidence that the integration exists and belongs to this
-- account -- an UPDATE that matches nothing is otherwise indistinguishable from success.
UPDATE integrations
SET credential_ciphertext = sqlc.arg(credential_ciphertext),
    wrapped_dek           = sqlc.arg(wrapped_dek),
    key_version           = sqlc.arg(key_version)
WHERE account_id = sqlc.arg(account_id)
  AND id = sqlc.arg(integration_id);

-- name: GetIntegrationCredential :one
SELECT credential_ciphertext, wrapped_dek, key_version, credential_verified_at
FROM integrations
WHERE account_id = sqlc.arg(account_id)
  AND id = sqlc.arg(integration_id);
