-- name: ListAccountPositions :many
-- Every folded position the account has, across every integration, joined to the identity
-- a reader can recognise. account_id is the primary defence (L12); RLS is the backstop.
--
-- The instrument is joined rather than resolved through the alias table: these rows are the
-- output of a fold that already resolved each event's symbol as of its own event_time
-- (L8, K22), so re-resolving here would be asking today's mapping a question that was
-- already answered correctly.
SELECT p.integration_id, p.instrument_id,
       i.canonical_symbol, i.kind,
       b.canonical_symbol AS base_asset,
       q.canonical_symbol AS quote_asset,
       p.quantity, p.avg_entry_price, p.realized_pnl, p.last_event_time
FROM positions p
JOIN instruments i ON i.id = p.instrument_id
JOIN assets b ON b.id = i.base_asset_id
JOIN assets q ON q.id = i.quote_asset_id
WHERE p.account_id = sqlc.arg(account_id)
ORDER BY p.integration_id, p.instrument_id;

-- name: ListAccountPositionFees :many
SELECT integration_id, instrument_id, fee_asset, amount
FROM position_fees
WHERE account_id = sqlc.arg(account_id)
ORDER BY integration_id, instrument_id, fee_asset;

-- name: ListLaggingIntegrations :many
-- Integrations holding at least one event the fold has not reached.
--
-- An EXISTS over the canonical order rather than a count: the question is "is there
-- anything after the cursor", which is an index seek, and it stays cheap on a ledger of any
-- size. Events that arrived *behind* the cursor are a different failure and are detected by
-- the projector itself, by row count (issue #4, K20).
--
-- No cursor row at all with events present is lagging by definition: nothing has been
-- folded yet.
SELECT i.id AS integration_id
FROM integrations i
WHERE i.account_id = sqlc.arg(account_id)
  AND EXISTS (
    SELECT 1
    FROM ledger_events e
    LEFT JOIN projection_cursors c
      ON c.account_id = e.account_id AND c.integration_id = e.integration_id
    WHERE e.account_id = i.account_id
      AND e.integration_id = i.id
      AND (c.integration_id IS NULL
           OR (e.event_time, e.venue_sequence, e.venue_event_id) >
              (c.last_event_time, c.last_venue_sequence, c.last_venue_event_id))
  )
ORDER BY i.id;
