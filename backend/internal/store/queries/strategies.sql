-- name: CreateStrategy :one
-- account_id is the primary defence (L12); RLS is the backstop underneath it.
INSERT INTO strategies (account_id, name, kind)
VALUES (sqlc.arg(account_id), sqlc.arg(name), sqlc.arg(kind))
RETURNING id;

-- name: ListStrategies :many
SELECT s.id, s.name, s.kind, s.created_at,
       COUNT(p.instrument_id)::bigint AS positions
FROM strategies s
LEFT JOIN position_strategies p
       ON p.account_id = s.account_id AND p.strategy_id = s.id
WHERE s.account_id = sqlc.arg(account_id)
GROUP BY s.id, s.name, s.kind, s.created_at
ORDER BY s.name;

-- name: AssignPositionStrategy :exec
-- Replaces rather than accumulates: the primary key is the position, so a second assignment
-- for the same position is the same row (K13 -- one strategy per position in V1).
INSERT INTO position_strategies (account_id, integration_id, instrument_id, strategy_id)
VALUES (sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(instrument_id),
        sqlc.arg(strategy_id))
ON CONFLICT (integration_id, instrument_id) DO UPDATE SET
  strategy_id = EXCLUDED.strategy_id,
  assigned_at = now();

-- name: ClearPositionStrategy :exec
DELETE FROM position_strategies
WHERE account_id = sqlc.arg(account_id)
  AND integration_id = sqlc.arg(integration_id)
  AND instrument_id = sqlc.arg(instrument_id);

-- name: ListPositionStrategies :many
-- Every tagged position for one account, with the strategy it belongs to, so a risk response
-- groups its positions in one read rather than one per position.
SELECT p.integration_id, p.instrument_id, s.id, s.name, s.kind
FROM position_strategies p
JOIN strategies s ON s.account_id = p.account_id AND s.id = p.strategy_id
WHERE p.account_id = sqlc.arg(account_id)
ORDER BY p.integration_id, p.instrument_id;

-- name: PositionExists :one
-- Tagging something that does not exist is a 404 rather than a row nobody can ever read: the
-- tag would sit there until the position appeared, and then apply to it silently.
SELECT EXISTS (
  SELECT 1 FROM positions
  WHERE account_id = sqlc.arg(account_id)
    AND integration_id = sqlc.arg(integration_id)
    AND instrument_id = sqlc.arg(instrument_id)
);
