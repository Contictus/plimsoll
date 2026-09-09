-- +goose Up

-- What things cost, at minute resolution (K7).
--
-- Reference data: BTCUSDT is BTCUSDT for every account, so there is no account_id and no
-- RLS -- the same deliberate exception assets and instruments take (00004, 00005).
--
-- It differs from them in one way that is worth stating rather than glossing: assets and
-- instruments are curated by the owner through migrations, so SELECT-only for the app role
-- is the whole story there. Prices are written at runtime by the worker, and the worker
-- connects as plimsoll_app like everything else. So the app role must hold INSERT and
-- UPDATE here, and "only the worker writes prices" is a property of the code, not of a
-- privilege.
--
-- That is a gap, and it is recorded rather than papered over: the schema has exactly two
-- roles (K15), and "reference data the application writes at runtime" is a third category
-- neither of them was shaped for. Closing it means a third role with its own DSN, which is
-- a decision to take deliberately and not as a side effect of adding a table.

--
-- Minute resolution, not every tick. K7 chose it and F9 confirms it survives contact with
-- the venue: klines are served at 1m, so a historical backfill and the live recorder write
-- the same shape of row and `?at=` cannot tell which one filled a given minute.
CREATE TABLE price_ticks (
  instrument_id BIGINT         NOT NULL REFERENCES instruments (id),

  -- The minute this price belongs to, truncated in UTC. Part of the key, which is what
  -- makes the minute the deduplication: a stream pushing every second writes one row per
  -- minute and the last price in it wins. A table that grew per push would be a different
  -- product's problem inside a week.
  ts            TIMESTAMPTZ    NOT NULL,

  price         NUMERIC(38,18) NOT NULL CHECK (price > 0),

  -- Which feed said so. Part of the audit trail a valuation run records (K11): "one
  -- valuation run per response" is only meaningful if the run can say where each leg came
  -- from.
  source        TEXT           NOT NULL CHECK (source <> ''),

  -- The venue's own timestamp for the observation, which is not the minute it was filed
  -- under. price_stale (L11) is computed from this: a price is as old as the venue says it
  -- is, never as old as the last time we looked at the socket (F7).
  observed_at   TIMESTAMPTZ    NOT NULL,

  PRIMARY KEY (instrument_id, ts)
);

-- BRIN rather than btree. The table is append-mostly and written in time order, which is
-- the one access pattern BRIN is for, at a fraction of the size — and every query that
-- matters here is a range over ts (K7).
CREATE INDEX price_ticks_ts_brin ON price_ticks USING brin (ts);

-- No DELETE. A price that was observed was observed; correcting one means writing the
-- corrected observation over it, and the observed_at guard in the upsert decides which of
-- two claims about the same minute wins. Removing history is not a repair.
GRANT SELECT, INSERT, UPDATE ON price_ticks TO plimsoll_app;

-- +goose Down
DROP TABLE price_ticks;
