-- name: ClaimIntegrationLease :one
-- The whole value of this statement is that it is one statement. A read-then-write would
-- let two workers both observe an expired lease and both take it; ON CONFLICT DO UPDATE
-- takes a row lock, so the loser re-evaluates its WHERE against the winner's committed row
-- and comes back with no rows at all (K20).
--
-- now() throughout: the database's clock decides, never the worker's. Two workers with
-- skewed clocks would otherwise disagree about whether the lease had expired.
--
-- Returning no row means "not claimed", which is a normal outcome and not an error.
--
-- ttl_seconds is an integer, not a double. sqlc maps a double precision parameter to Go's
-- 64-bit floating point type, and internal/store keeps a test that refuses that type
-- anywhere in generated code rather than arguing case by case about which values are money
-- (L1). A lease measured in whole seconds loses nothing by obeying it.
INSERT INTO integration_leases (
  account_id, integration_id, owner_id, acquired_at, expires_at
) VALUES (
  sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(owner_id),
  now(), now() + make_interval(secs => sqlc.arg(ttl_seconds)::int)
)
ON CONFLICT (integration_id) DO UPDATE SET
  owner_id    = EXCLUDED.owner_id,
  acquired_at = EXCLUDED.acquired_at,
  expires_at  = EXCLUDED.expires_at
WHERE integration_leases.expires_at < now()
   OR integration_leases.owner_id = EXCLUDED.owner_id
RETURNING owner_id, expires_at;

-- name: HeartbeatIntegrationLease :one
-- Extends a lease the caller still holds. The expires_at check is not redundant with the
-- owner check: a worker that stopped heartbeating long enough for its lease to lapse has
-- lost it whether or not anyone else has taken it yet, and letting it resume writing is the
-- two-writer race the lease exists to prevent.
UPDATE integration_leases
SET expires_at = now() + make_interval(secs => sqlc.arg(ttl_seconds)::int)
WHERE account_id = sqlc.arg(account_id)
  AND integration_id = sqlc.arg(integration_id)
  AND owner_id = sqlc.arg(owner_id)
  AND expires_at > now()
RETURNING expires_at;

-- name: ReleaseIntegrationLease :execrows
-- Scoped by owner: a worker may only give up its own lease. Releasing another's would hand
-- the integration to a third process while the second was still writing.
DELETE FROM integration_leases
WHERE account_id = sqlc.arg(account_id)
  AND integration_id = sqlc.arg(integration_id)
  AND owner_id = sqlc.arg(owner_id);

-- name: LeaseIsHeldBy :one
-- Read inside the same transaction as the write it guards. A worker that checked its lease
-- and then wrote in a second transaction would have a window between the two for the lease
-- to be lost in, and the whole point of a lease is that there is no such window.
SELECT EXISTS (
  SELECT 1 FROM integration_leases
  WHERE account_id = sqlc.arg(account_id)
    AND integration_id = sqlc.arg(integration_id)
    AND owner_id = sqlc.arg(owner_id)
    AND expires_at > now()
);
