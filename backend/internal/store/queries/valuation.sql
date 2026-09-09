-- name: ListPricedPairs :many
-- Every instrument that has a price, with the two assets it connects and the newest tick
-- for it. This is the graph the valuation walk runs over.
--
-- DISTINCT ON is the one Postgres idiom that takes the newest row per instrument in a
-- single index-ordered pass rather than a self-join per instrument.
SELECT DISTINCT ON (p.instrument_id)
       p.instrument_id, i.base_asset_id, i.quote_asset_id, p.price, p.observed_at
FROM price_ticks p
JOIN instruments i ON i.id = p.instrument_id
WHERE p.ts <= sqlc.arg(at)::timestamptz
ORDER BY p.instrument_id, p.ts DESC;

-- name: ListAssetIDs :many
-- Every asset in the registry. A run prices all of them rather than only the ones some
-- account holds, because a run belongs to no account (00019) -- and because an asset that
-- was unpriceable is worth recording as unpriceable exactly once, not once per reader.
SELECT id FROM assets ORDER BY id;

-- name: InsertValuationRun :one
INSERT INTO valuation_runs (as_of, price_source, assumed_peg, oldest_observed_at)
VALUES (sqlc.arg(as_of), sqlc.arg(price_source), sqlc.arg(assumed_peg),
        sqlc.narg(oldest_observed_at))
RETURNING id;

-- name: InsertValuationPrice :exec
INSERT INTO valuation_prices (run_id, asset_id, price_usd, path, assumed_peg, observed_at)
VALUES (sqlc.arg(run_id), sqlc.arg(asset_id), sqlc.arg(price_usd), sqlc.arg(path),
        sqlc.arg(assumed_peg), sqlc.narg(observed_at));

-- name: GetLatestValuationRun :one
-- The newest run at or before an instant.
--
-- At or before, never after: a response for a past instant must not be built on prices
-- from after it. That is look-ahead bias, and in a risk product it is the bug that makes a
-- backtest look brilliant.
SELECT id, as_of, numeraire, price_source, assumed_peg, oldest_observed_at, created_at
FROM valuation_runs
WHERE as_of <= sqlc.arg(at)::timestamptz
ORDER BY as_of DESC, id DESC
LIMIT 1;

-- name: ListValuationPrices :many
SELECT asset_id, price_usd, path, assumed_peg, observed_at
FROM valuation_prices
WHERE run_id = sqlc.arg(run_id)
ORDER BY asset_id;

-- name: CountValuationRuns :one
SELECT count(*) FROM valuation_runs;

-- name: GetAssetIDBySymbol :one
-- The canonical registry is keyed by symbol for exactly this: configuration names assets
-- the way a human does, and the id it resolves to is a database detail.
SELECT id FROM assets WHERE canonical_symbol = sqlc.arg(canonical_symbol);
