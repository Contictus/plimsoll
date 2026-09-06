-- name: ListPositionEventsAfter :many
-- Every event that folds into one position, in canonical order (L7), from a keyset cursor.
--
-- Paged rather than fetched whole: the lineage of a five-year position is not bounded by
-- anything, and holding all of it in memory to render the last page of it would make the
-- endpoint's cost a property of the account's history rather than of the request.
SELECT seq, venue_event_id, venue_sequence, source, event_type, instrument_id, asset_id,
       strategy_id, side, quantity, price, fee, fee_asset, event_time, ingested_at, raw
FROM ledger_events
WHERE account_id = sqlc.arg(account_id)
  AND integration_id = sqlc.arg(integration_id)
  AND instrument_id = sqlc.arg(instrument_id)
  AND (event_time, venue_sequence, venue_event_id) >
      (sqlc.arg(after_event_time)::timestamptz,
       sqlc.arg(after_venue_sequence)::bigint,
       sqlc.arg(after_venue_event_id)::text)
ORDER BY event_time, venue_sequence, venue_event_id
LIMIT sqlc.arg(max_rows);

-- name: StreamAccountEvents :many
-- The account's ledger across every integration, in canonical order with integration_id as
-- the last tiebreak -- two integrations can mint the same venue_event_id, and a cursor that
-- could not tell them apart would stop at the first of them forever.
--
-- Never paginated on seq: identity values are assigned before commit, so a seq cursor can
-- skip a row that was still in flight (K20, K41). seq is returned as lineage, not as a
-- position in the listing.
SELECT e.seq, e.integration_id, e.venue_event_id, e.venue_sequence, e.source, e.event_type,
       e.instrument_id, e.asset_id, e.side, e.quantity, e.price, e.fee, e.fee_asset,
       e.event_time, e.ingested_at,
       i.canonical_symbol AS instrument_symbol,
       a.canonical_symbol AS asset_symbol
FROM ledger_events e
LEFT JOIN instruments i ON i.id = e.instrument_id
LEFT JOIN assets a ON a.id = e.asset_id
WHERE e.account_id = sqlc.arg(account_id)
  AND (e.event_time, e.venue_sequence, e.venue_event_id, e.integration_id) >
      (sqlc.arg(after_event_time)::timestamptz,
       sqlc.arg(after_venue_sequence)::bigint,
       sqlc.arg(after_venue_event_id)::text,
       sqlc.arg(after_integration_id)::uuid)
ORDER BY e.event_time, e.venue_sequence, e.venue_event_id, e.integration_id
LIMIT sqlc.arg(max_rows);
