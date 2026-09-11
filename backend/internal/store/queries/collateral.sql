-- name: UpsertCollateralSnapshot :exec
-- Latest-only, replaced wholesale. A snapshot is the exchange's answer about one instant and
-- there is no arithmetic that combines two of them, so there is nothing to merge: the new
-- one is the answer and the old one was.
INSERT INTO collateral_snapshots (
  account_id, integration_id, as_of, captured_at,
  margin_balance, wallet_balance, unrealized_pnl, maintenance_margin, available_balance
) VALUES (
  sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(as_of), sqlc.arg(captured_at),
  sqlc.arg(margin_balance), sqlc.arg(wallet_balance), sqlc.arg(unrealized_pnl),
  sqlc.arg(maintenance_margin), sqlc.arg(available_balance)
)
ON CONFLICT (integration_id) DO UPDATE SET
  as_of              = EXCLUDED.as_of,
  captured_at        = EXCLUDED.captured_at,
  margin_balance     = EXCLUDED.margin_balance,
  wallet_balance     = EXCLUDED.wallet_balance,
  unrealized_pnl     = EXCLUDED.unrealized_pnl,
  maintenance_margin = EXCLUDED.maintenance_margin,
  available_balance  = EXCLUDED.available_balance;

-- name: DeleteCollateralPositions :exec
-- Cleared before each capture's rows go in, inside one transaction. A position that closed
-- between two captures has no row in the new one, and merging instead of replacing would
-- leave it on the screen forever -- a closed position still showing a liquidation distance,
-- which is the most alarming possible way to be wrong.
DELETE FROM collateral_positions
WHERE account_id = sqlc.arg(account_id) AND integration_id = sqlc.arg(integration_id);

-- name: InsertCollateralPosition :exec
INSERT INTO collateral_positions (
  account_id, integration_id, instrument_id,
  quantity, entry_price, mark_price, liquidation_price, notional, leverage, maint_margin
) VALUES (
  sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(instrument_id),
  sqlc.arg(quantity), sqlc.arg(entry_price), sqlc.arg(mark_price),
  sqlc.arg(liquidation_price), sqlc.arg(notional), sqlc.arg(leverage), sqlc.arg(maint_margin)
);

-- name: UpsertLeverageBracket :exec
-- Upserted rather than replaced: the tier table changes rarely and a capture that fetched
-- fewer symbols than the last one must not delete the tiers of a position it did not ask
-- about. A stale bracket is a number M7.5 can still shock; a missing one is an error it
-- cannot answer through.
INSERT INTO leverage_brackets (
  account_id, integration_id, instrument_id, bracket,
  notional_floor, notional_cap, maint_margin_ratio, cum, captured_at
) VALUES (
  sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(instrument_id), sqlc.arg(bracket),
  sqlc.arg(notional_floor), sqlc.arg(notional_cap), sqlc.arg(maint_margin_ratio),
  sqlc.arg(cum), sqlc.arg(captured_at)
)
ON CONFLICT (integration_id, instrument_id, bracket) DO UPDATE SET
  notional_floor     = EXCLUDED.notional_floor,
  notional_cap       = EXCLUDED.notional_cap,
  maint_margin_ratio = EXCLUDED.maint_margin_ratio,
  cum                = EXCLUDED.cum,
  captured_at        = EXCLUDED.captured_at;

-- name: GetCollateralSnapshot :one
-- account_id is the primary defence (L12); RLS is the backstop underneath it.
SELECT integration_id, as_of, captured_at, margin_balance, wallet_balance,
       unrealized_pnl, maintenance_margin, available_balance
FROM collateral_snapshots
WHERE account_id = sqlc.arg(account_id) AND integration_id = sqlc.arg(integration_id);

-- name: ListAccountCollateral :many
-- Every integration's snapshot for one account, so a risk response is one read rather than
-- one per connection.
SELECT s.integration_id, s.as_of, s.captured_at, s.margin_balance, s.wallet_balance,
       s.unrealized_pnl, s.maintenance_margin, s.available_balance
FROM collateral_snapshots s
WHERE s.account_id = sqlc.arg(account_id)
ORDER BY s.integration_id;

-- name: ListCollateralPositions :many
-- base_symbol is the asset the contract is ON, which is what a scenario shock names and what
-- a spot holding of the same asset is keyed by. Without it a hedged book would be shocked on
-- one leg only (K56).
SELECT p.integration_id, p.instrument_id, i.canonical_symbol, b.canonical_symbol AS base_symbol,
       p.quantity, p.entry_price, p.mark_price, p.liquidation_price,
       p.notional, p.leverage, p.maint_margin
FROM collateral_positions p
JOIN instruments i ON i.id = p.instrument_id
JOIN assets b ON b.id = i.base_asset_id
WHERE p.account_id = sqlc.arg(account_id)
ORDER BY p.integration_id, i.canonical_symbol;

-- name: ListLeverageBrackets :many
SELECT integration_id, instrument_id, bracket, notional_floor, notional_cap,
       maint_margin_ratio, cum
FROM leverage_brackets
WHERE account_id = sqlc.arg(account_id) AND instrument_id = sqlc.arg(instrument_id)
ORDER BY notional_floor;

-- name: ListAccountLeverageBrackets :many
-- Every bracket table this account has captured, in one read.
--
-- One read rather than one per position on purpose: a scenario touching twenty symbols would
-- otherwise make twenty round trips inside one request, which is the shape that has already
-- cost this project a connection-slot exhaustion once.
SELECT integration_id, instrument_id, bracket, notional_floor, notional_cap,
       maint_margin_ratio, cum
FROM leverage_brackets
WHERE account_id = sqlc.arg(account_id)
ORDER BY instrument_id, notional_floor;
