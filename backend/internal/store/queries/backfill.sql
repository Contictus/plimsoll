-- name: GetBackfillProgress :one
-- Scoped by account_id as the primary defence (L12); RLS is the backstop underneath it.
SELECT scope, cursor, completed_at
FROM backfill_progress
WHERE account_id = sqlc.arg(account_id)
  AND integration_id = sqlc.arg(integration_id)
  AND scope = sqlc.arg(scope);

-- name: UpsertBackfillProgress :exec
-- The write a walker makes in the same transaction as the events the cursor describes.
INSERT INTO backfill_progress (
  account_id, integration_id, scope, cursor, completed_at
) VALUES (
  sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(scope),
  sqlc.arg(cursor), sqlc.narg(completed_at)
)
ON CONFLICT (integration_id, scope) DO UPDATE SET
  cursor       = EXCLUDED.cursor,
  completed_at = EXCLUDED.completed_at,
  updated_at   = now();

-- name: OpenBackfillScope :exec
-- Records that a scope has work to do without disturbing one already underway. Discovery
-- uses it to hand each traded symbol to the trade walk; a rerun must not rewind a walk
-- that has since made progress, so the conflict does nothing rather than resetting.
INSERT INTO backfill_progress (account_id, integration_id, scope)
VALUES (sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(scope))
ON CONFLICT (integration_id, scope) DO NOTHING;

-- name: ListBackfillProgress :many
-- Ordered by scope so that a caller resuming a sweep sees them in the same order twice.
SELECT scope, cursor, completed_at
FROM backfill_progress
WHERE account_id = sqlc.arg(account_id)
  AND integration_id = sqlc.arg(integration_id)
  AND scope LIKE sqlc.arg(scope_prefix)
ORDER BY scope;
