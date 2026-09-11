-- name: GetInstrumentLegs :one
-- The two assets an instrument is made of. Read by id rather than by symbol: the event
-- already carries the instrument the alias resolved to at its own event_time (L8), and
-- going back through the symbol here would re-ask a question already answered correctly.
SELECT base_asset_id, quote_asset_id FROM instruments WHERE id = sqlc.arg(instrument_id);

-- name: ListAssetBalances :many
SELECT asset_id, quantity, last_event_time, last_venue_sequence, last_venue_event_id
FROM asset_balances
WHERE account_id = sqlc.arg(account_id) AND integration_id = sqlc.arg(integration_id)
ORDER BY asset_id;

-- name: UpsertAssetBalance :exec
INSERT INTO asset_balances (
  account_id, integration_id, asset_id, quantity,
  last_event_time, last_venue_sequence, last_venue_event_id
) VALUES (
  sqlc.arg(account_id), sqlc.arg(integration_id), sqlc.arg(asset_id), sqlc.arg(quantity),
  sqlc.arg(last_event_time), sqlc.arg(last_venue_sequence), sqlc.arg(last_venue_event_id)
)
ON CONFLICT (integration_id, asset_id) DO UPDATE SET
  quantity            = EXCLUDED.quantity,
  last_event_time     = EXCLUDED.last_event_time,
  last_venue_sequence = EXCLUDED.last_venue_sequence,
  last_venue_event_id = EXCLUDED.last_venue_event_id,
  updated_at          = now();

-- name: DropAssetBalances :exec
DELETE FROM asset_balances
WHERE account_id = sqlc.arg(account_id) AND integration_id = sqlc.arg(integration_id);

-- name: ListAccountBalances :many
-- Every balance the account holds, across every integration, with the ticker a reader
-- recognises rather than an id only the schema knows.
SELECT b.integration_id, b.asset_id, a.canonical_symbol, b.quantity, b.last_event_time
FROM asset_balances b
JOIN assets a ON a.id = b.asset_id
WHERE b.account_id = sqlc.arg(account_id)
ORDER BY a.canonical_symbol, b.integration_id;

-- name: ListIntegrationsWithUnattributedFees :many
-- Integrations holding a fee whose asset never resolved. The fee was still stored -- losing
-- a fill to keep a fee would be the worse trade -- so the balance for that asset is short
-- by exactly these amounts, and the reader is told rather than left to discover it during a
-- reconciliation (K22, L11).
SELECT DISTINCT integration_id
FROM ledger_events
WHERE account_id = sqlc.arg(account_id)
  AND fee IS NOT NULL AND fee <> 0
  AND fee_asset_id IS NULL
ORDER BY integration_id;

-- name: ListIntegrationBalancesWithSymbol :many
-- One integration's asset fold, carrying the canonical symbol the venue's code resolves to.
-- Reconciliation compares against this: the venue's own code is resolved through the alias
-- table at the snapshot's instant and never used as a key itself (L8, K22).
SELECT b.asset_id, a.canonical_symbol, b.quantity
FROM asset_balances b
JOIN assets a ON a.id = b.asset_id
WHERE b.account_id = sqlc.arg(account_id)
  AND b.integration_id = sqlc.arg(integration_id)
ORDER BY a.canonical_symbol;

-- name: SumUnattributedFeesByAsset :many
-- How much of each asset left the account as a fee we could not attribute.
--
-- The fee was stored with its venue asset code but no asset id, because the code resolved to
-- nothing (K22). Our balance for that asset is therefore HIGH by exactly this much -- which
-- makes this the one piece of evidence that can classify such a gap as `unsupported` rather
-- than as a missing event we cannot account for (K54).
SELECT fee_asset, SUM(fee)::numeric(38,18) AS total
FROM ledger_events
WHERE account_id = sqlc.arg(account_id)
  AND integration_id = sqlc.arg(integration_id)
  AND fee IS NOT NULL AND fee <> 0
  AND fee_asset_id IS NULL
  AND fee_asset IS NOT NULL
GROUP BY fee_asset
ORDER BY fee_asset;

-- name: ListVenueIdentitiesSeenTwice :many
-- Venue event ids this account holds under more than one integration.
--
-- L5's dedup key is UNIQUE (integration_id, venue_event_id), so one trade cannot enter twice
-- through one connection. It CAN enter twice through two -- the same sub-account connected
-- under two integrations -- and that is the one way a position doubles without any single
-- ingest path being wrong. It is therefore the only duplicate this classifier can prove.
SELECT venue_event_id, count(DISTINCT integration_id) AS integrations
FROM ledger_events
WHERE account_id = sqlc.arg(account_id)
GROUP BY venue_event_id
HAVING count(DISTINCT integration_id) > 1
ORDER BY venue_event_id;
