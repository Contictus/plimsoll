-- name: CreateAlertChannel :one
INSERT INTO alert_channels (
  account_id, kind, label, enabled, config_ciphertext, wrapped_dek, key_version
) VALUES (
  sqlc.arg(account_id), sqlc.arg(kind), sqlc.arg(label), sqlc.arg(enabled),
  sqlc.arg(config_ciphertext), sqlc.arg(wrapped_dek), sqlc.arg(key_version)
)
RETURNING id;

-- name: ListAlertChannels :many
-- The sealed config comes back with the row: every caller that lists channels is about to
-- send through them, and a second query to fetch the secret would be a second place to get
-- the account scoping wrong.
SELECT id, kind, label, enabled, config_ciphertext, wrapped_dek, key_version, created_at
FROM alert_channels
WHERE account_id = sqlc.arg(account_id)
ORDER BY label;

-- name: DeleteAlertChannel :execrows
DELETE FROM alert_channels
WHERE account_id = sqlc.arg(account_id) AND id = sqlc.arg(id);
