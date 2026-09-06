-- name: UpsertIntegrationStatus :exec
-- since is only moved when the state actually changes. A worker republishing "degraded"
-- every heartbeat must not keep resetting how long it has been degraded -- that would make
-- a feed down for an hour report as down for a few seconds, every time anyone looked.
INSERT INTO integration_status (account_id, integration_id, state, owner_id, since)
VALUES (sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(state),
        sqlc.arg(owner_id), sqlc.arg(since))
ON CONFLICT (integration_id) DO UPDATE SET
  state      = EXCLUDED.state,
  owner_id   = EXCLUDED.owner_id,
  since      = CASE WHEN integration_status.state = EXCLUDED.state
                    THEN integration_status.since ELSE EXCLUDED.since END,
  updated_at = now();

-- name: ListIntegrationStatus :many
-- Every integration the account has, whether or not a worker has ever reported on it: a
-- LEFT JOIN rather than a lookup, because an integration with no status row is exactly the
-- case a reader must be told about and an inner join would hide it.
SELECT i.id AS integration_id, i.exchange, i.label, i.status AS configured_status,
       s.state, s.owner_id, s.since, s.updated_at
FROM integrations i
LEFT JOIN integration_status s
  ON s.account_id = i.account_id AND s.integration_id = i.id
WHERE i.account_id = sqlc.arg(account_id)
ORDER BY i.created_at, i.id;
