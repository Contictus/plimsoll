-- name: ResolveInstrumentAlias :one
-- Market is part of the lookup, not a filter applied afterwards (K10). The exclusion
-- constraint guarantees at most one row matches, so :one is exact.
SELECT instrument_id
FROM instrument_aliases
WHERE exchange = sqlc.arg(exchange)
  AND market = sqlc.arg(market)
  AND exchange_symbol = sqlc.arg(exchange_symbol)
  AND validity @> sqlc.arg(at)::timestamptz;
