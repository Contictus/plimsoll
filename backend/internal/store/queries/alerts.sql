-- name: ListAlertRules :many
-- account_id is the primary defence (L12); RLS is the backstop underneath it.
SELECT r.id, r.name, r.metric, r.scope_kind, r.scope_name, r.comparator,
       r.trigger_at, r.clear_at, r.cooldown_seconds, r.enabled, r.created_at,
       s.firing, s.since, s.last_fired_at
FROM alert_rules r
LEFT JOIN alert_state s ON s.rule_id = r.id
WHERE r.account_id = sqlc.arg(account_id)
ORDER BY r.name;

-- name: CreateAlertRule :one
INSERT INTO alert_rules (
  account_id, name, metric, scope_kind, scope_name, comparator,
  trigger_at, clear_at, cooldown_seconds, enabled
) VALUES (
  sqlc.arg(account_id), sqlc.arg(name), sqlc.arg(metric), sqlc.arg(scope_kind),
  sqlc.arg(scope_name), sqlc.arg(comparator), sqlc.arg(trigger_at), sqlc.arg(clear_at),
  sqlc.arg(cooldown_seconds), sqlc.arg(enabled)
)
RETURNING id;

-- name: UpdateAlertRule :execrows
-- The thresholds are the whole of what a user tunes, so an update replaces them together: a
-- trigger changed without its clear line is how a band ends up pointing the wrong way, and
-- the schema would then refuse the write with no obvious cause.
UPDATE alert_rules SET
  name = sqlc.arg(name),
  comparator = sqlc.arg(comparator),
  trigger_at = sqlc.arg(trigger_at),
  clear_at = sqlc.arg(clear_at),
  cooldown_seconds = sqlc.arg(cooldown_seconds),
  enabled = sqlc.arg(enabled)
WHERE account_id = sqlc.arg(account_id) AND id = sqlc.arg(id);

-- name: UpsertAlertState :exec
INSERT INTO alert_state (rule_id, account_id, firing, since, last_fired_at)
VALUES (sqlc.arg(rule_id), sqlc.arg(account_id), sqlc.arg(firing), sqlc.arg(since),
        sqlc.arg(last_fired_at))
ON CONFLICT (rule_id) DO UPDATE SET
  firing = EXCLUDED.firing,
  since = EXCLUDED.since,
  last_fired_at = EXCLUDED.last_fired_at;

-- name: InsertAlert :one
-- The record, written whether or not the message ever leaves the building: delivery is
-- transport, and an account whose channel expired still has a history of what happened while
-- nobody was being told.
INSERT INTO alerts (
  account_id, rule_id, kind, metric, scope_kind, scope_name, value, run_id, fired_at
) VALUES (
  sqlc.arg(account_id), sqlc.arg(rule_id), sqlc.arg(kind), sqlc.arg(metric),
  sqlc.arg(scope_kind), sqlc.arg(scope_name), sqlc.arg(value), sqlc.arg(run_id),
  sqlc.arg(fired_at)
)
RETURNING id;

-- name: MarkAlertDelivered :exec
UPDATE alerts SET delivered_at = sqlc.arg(delivered_at), delivery_error = NULL
WHERE account_id = sqlc.arg(account_id) AND id = sqlc.arg(id);

-- name: MarkAlertUndelivered :exec
-- The error is written by a caller that has already redacted it: whatever went wrong with a
-- channel, the channel's own credential is not what goes in this column (L13).
UPDATE alerts SET delivery_error = sqlc.arg(delivery_error)
WHERE account_id = sqlc.arg(account_id) AND id = sqlc.arg(id);

-- name: ListAlerts :many
SELECT a.id, a.rule_id, r.name AS rule_name, a.kind, a.metric, a.scope_kind, a.scope_name,
       a.value, a.run_id, a.fired_at, a.delivered_at, a.delivery_error
FROM alerts a
JOIN alert_rules r ON r.id = a.rule_id
WHERE a.account_id = sqlc.arg(account_id)
ORDER BY a.fired_at DESC
LIMIT sqlc.arg(max_rows);
