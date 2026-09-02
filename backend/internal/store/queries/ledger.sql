-- name: InsertLedgerEvent :execrows
-- ON CONFLICT DO NOTHING is the deduplication mechanism, and it is correct only because
-- venue_event_id is built from exchange fields alone (K19, L5). :execrows so the caller
-- can report how many of the events it offered were actually new.
INSERT INTO ledger_events (
  account_id, integration_id, venue_event_id, venue_sequence, source, event_type,
  instrument_id, strategy_id, side, quantity, price, fee, fee_asset, event_time, raw
) VALUES (
  sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(venue_event_id),
  sqlc.arg(venue_sequence), sqlc.arg(source), sqlc.arg(event_type),
  sqlc.narg(instrument_id), sqlc.narg(strategy_id), sqlc.narg(side),
  sqlc.narg(quantity), sqlc.narg(price), sqlc.narg(fee), sqlc.narg(fee_asset),
  sqlc.arg(event_time), sqlc.arg(raw)
)
ON CONFLICT (integration_id, venue_event_id) DO NOTHING;

-- name: StreamLedgerEvents :many
-- account_id is the primary defence (L12); RLS is the backstop underneath it. The row
-- comparison is the canonical order (L7) used as a keyset cursor -- never the global seq,
-- whose values are assigned before commit and can therefore be observed out of order
-- (K20, L6).
SELECT seq, account_id, integration_id, venue_event_id, venue_sequence, source,
       event_type, instrument_id, strategy_id, side, quantity, price, fee, fee_asset,
       event_time, ingested_at, raw
FROM ledger_events
WHERE account_id = sqlc.arg(account_id)
  AND integration_id = sqlc.arg(integration_id)
  AND (event_time, venue_sequence, venue_event_id) >
      (sqlc.arg(after_event_time)::timestamptz,
       sqlc.arg(after_venue_sequence)::bigint,
       sqlc.arg(after_venue_event_id)::text)
ORDER BY event_time, venue_sequence, venue_event_id
LIMIT sqlc.arg(max_rows);
