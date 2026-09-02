-- name: ResolveAssetAlias :one
-- K22/L8: resolution happens at the event's own time, never with today's mapping. The
-- exclusion constraint guarantees at most one row matches, so :one is exact rather than
-- a truncation of several candidates.
SELECT asset_id
FROM asset_aliases
WHERE source = sqlc.arg(source)
  AND external_symbol = sqlc.arg(external_symbol)
  AND validity @> sqlc.arg(at)::timestamptz;
