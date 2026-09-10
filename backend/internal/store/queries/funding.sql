-- name: SumFundingBySymbol :many
-- What funding cost or paid, per instrument, over a half-open window [from, to).
--
-- Summed in the database rather than folded in Go because there is no state to carry: this
-- is arithmetic over rows the ledger already holds, and streaming a year of payments through
-- the API process to add them up would make the endpoint's cost a property of how long the
-- account has been trading.
--
-- The asset is the instrument's QUOTE asset, which is what the balance fold credits a
-- funding payment to (balance.Deltas). V1 is USD-M only, where the settle asset and the
-- quote asset are the same one; taking settle_asset_id here instead would be correct in
-- isolation and would silently disagree with the fold the day coin-M arrives.
SELECT e.integration_id,
       e.instrument_id,
       i.canonical_symbol,
       a.canonical_symbol AS asset,
       SUM(e.quantity)::numeric(38,18) AS total,
       COUNT(*)::bigint                AS events,
       MIN(e.event_time)::timestamptz  AS first_event_time,
       MAX(e.event_time)::timestamptz  AS last_event_time
FROM ledger_events e
JOIN instruments i ON i.id = e.instrument_id
JOIN assets a      ON a.id = i.quote_asset_id
WHERE e.account_id = sqlc.arg(account_id)
  AND e.event_type = 'FUNDING_PAYMENT'
  AND e.event_time >= sqlc.arg(from_time)
  AND e.event_time <  sqlc.arg(to_time)
GROUP BY e.integration_id, e.instrument_id, i.canonical_symbol, a.canonical_symbol
ORDER BY i.canonical_symbol, e.integration_id;
