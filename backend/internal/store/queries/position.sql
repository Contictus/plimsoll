-- name: ListPositions :many
SELECT instrument_id, quantity, avg_entry_price, realized_pnl,
       last_event_time, last_venue_sequence, last_venue_event_id
FROM positions
WHERE account_id = sqlc.arg(account_id) AND integration_id = sqlc.arg(integration_id)
ORDER BY instrument_id;

-- name: ListPositionFees :many
SELECT instrument_id, fee_asset, amount
FROM position_fees
WHERE account_id = sqlc.arg(account_id) AND integration_id = sqlc.arg(integration_id)
ORDER BY instrument_id, fee_asset;

-- name: UpsertPosition :exec
INSERT INTO positions (
  account_id, integration_id, instrument_id, quantity, avg_entry_price, realized_pnl,
  last_event_time, last_venue_sequence, last_venue_event_id
) VALUES (
  sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(instrument_id),
  sqlc.arg(quantity), sqlc.arg(avg_entry_price), sqlc.arg(realized_pnl),
  sqlc.arg(last_event_time), sqlc.arg(last_venue_sequence), sqlc.arg(last_venue_event_id)
)
ON CONFLICT (integration_id, instrument_id) DO UPDATE SET
  quantity            = EXCLUDED.quantity,
  avg_entry_price     = EXCLUDED.avg_entry_price,
  realized_pnl        = EXCLUDED.realized_pnl,
  last_event_time     = EXCLUDED.last_event_time,
  last_venue_sequence = EXCLUDED.last_venue_sequence,
  last_venue_event_id = EXCLUDED.last_venue_event_id,
  updated_at          = now();

-- name: DeletePositionFees :exec
DELETE FROM position_fees
WHERE account_id = sqlc.arg(account_id)
  AND integration_id = sqlc.arg(integration_id)
  AND instrument_id = sqlc.arg(instrument_id);

-- name: InsertPositionFee :exec
INSERT INTO position_fees (account_id, integration_id, instrument_id, fee_asset, amount)
VALUES (sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(instrument_id),
        sqlc.arg(fee_asset), sqlc.arg(amount));

-- name: GetProjectionCursor :one
SELECT last_event_time, last_venue_sequence, last_venue_event_id
FROM projection_cursors
WHERE account_id = sqlc.arg(account_id) AND integration_id = sqlc.arg(integration_id);

-- name: UpsertProjectionCursor :exec
INSERT INTO projection_cursors (
  account_id, integration_id, last_event_time, last_venue_sequence, last_venue_event_id
) VALUES (
  sqlc.arg(account_id), sqlc.arg(integration_id),
  sqlc.arg(last_event_time), sqlc.arg(last_venue_sequence), sqlc.arg(last_venue_event_id)
)
ON CONFLICT (integration_id) DO UPDATE SET
  last_event_time     = EXCLUDED.last_event_time,
  last_venue_sequence = EXCLUDED.last_venue_sequence,
  last_venue_event_id = EXCLUDED.last_venue_event_id,
  updated_at          = now();

-- name: DropProjection :exec
-- The projection only. position_fees cascades from positions, and position_strategies is
-- deliberately absent: the tag is user input and survives a rebuild (D2).
DELETE FROM positions
WHERE account_id = sqlc.arg(account_id) AND integration_id = sqlc.arg(integration_id);

-- name: DropProjectionCursor :exec
DELETE FROM projection_cursors
WHERE account_id = sqlc.arg(account_id) AND integration_id = sqlc.arg(integration_id);
