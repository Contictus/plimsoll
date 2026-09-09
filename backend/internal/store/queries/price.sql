-- name: UpsertPriceTick :exec
-- The minute is the key, so the last price observed in a minute wins. A stream pushing
-- every second therefore leaves one row per minute rather than sixty (K7).
--
-- The guard on observed_at is what makes that true under an out-of-order frame: a delayed
-- push carrying an older observation must not rewind a price the venue never moved back.
INSERT INTO price_ticks (instrument_id, ts, price, source, observed_at)
VALUES (sqlc.arg(instrument_id), sqlc.arg(ts), sqlc.arg(price), sqlc.arg(source),
        sqlc.arg(observed_at))
ON CONFLICT (instrument_id, ts) DO UPDATE SET
  price       = EXCLUDED.price,
  source      = EXCLUDED.source,
  observed_at = EXCLUDED.observed_at
WHERE EXCLUDED.observed_at > price_ticks.observed_at;

-- name: GetPriceTickAt :one
-- The price in force at an instant: the newest tick at or before it.
--
-- At or before, never after. Reaching forward to the next price is look-ahead bias, and in
-- a risk product it is the bug that makes a backtest look brilliant and a liquidation
-- arrive unannounced.
SELECT instrument_id, ts, price, source, observed_at
FROM price_ticks
WHERE instrument_id = sqlc.arg(instrument_id)
  AND ts <= sqlc.arg(at)::timestamptz
ORDER BY ts DESC
LIMIT 1;

-- name: ListLatestPriceTicks :many
-- The newest tick per instrument. DISTINCT ON is the one Postgres idiom that answers this
-- in a single index-ordered pass rather than a self-join per instrument.
SELECT DISTINCT ON (instrument_id)
       instrument_id, ts, price, source, observed_at
FROM price_ticks
ORDER BY instrument_id, ts DESC;

-- name: CountPriceTicks :one
SELECT count(*) FROM price_ticks WHERE instrument_id = sqlc.arg(instrument_id);
