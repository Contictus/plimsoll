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

-- name: ReopenBackfillScopes :execrows
-- Rewinds every scope of one integration so the history walk runs again (K55).
--
-- This is the whole of "resync". It writes no correction and touches no ledger row (L2):
-- anything genuinely missing is appended by the ordinary ingest path under the ordinary dedup
-- key, and anything already present is deduplicated away (L5) -- which is what makes rewinding
-- to the beginning safe rather than reckless.
--
-- The cursor goes to '' and not to NULL: the column is NOT NULL precisely so that "nothing
-- walked yet" has one spelling rather than two (migration 00013).
UPDATE backfill_progress
   SET cursor = '', completed_at = NULL, updated_at = now()
 WHERE account_id = sqlc.arg(account_id)
   AND integration_id = sqlc.arg(integration_id);
