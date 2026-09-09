-- +goose Up

-- One valuation run per response (K11, L10), and everything a total depends on recorded
-- with it.
--
-- A run is ACCOUNT-INDEPENDENT, which is the property that makes K11 true rather than
-- merely intended. It prices assets, not positions: a total is that run's prices folded
-- against one account's holdings at read time. So two accounts reading the same instant
-- read the same run and cannot disagree about what a BTC was worth -- and neither can two
-- tabs belonging to one user. There is no account_id and no RLS here for the same reason
-- there is none on price_ticks: the rows are not about anybody.
CREATE TABLE valuation_runs (
  id           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

  -- The instant the run speaks for. Not created_at: a run rebuilt for a past instant is
  -- as_of that instant and created now, and conflating them would make a historical
  -- answer look like a current one.
  as_of        TIMESTAMPTZ NOT NULL,

  -- USD, and checked. K17 fixed the numeraire, and a second one appearing in this column
  -- would silently make two rows incomparable while every query still worked.
  numeraire    TEXT        NOT NULL DEFAULT 'USD' CHECK (numeraire = 'USD'),

  price_source TEXT        NOT NULL CHECK (price_source <> ''),

  -- True when any leg of any path fell back to an assumed rate. With one venue and no
  -- fiat market that is every run M4 can produce, which is a truthful statement about a
  -- real limitation rather than a useless column: the path in valuation_prices says which
  -- asset was assumed, and the day a real USD source exists this starts telling runs apart.
  assumed_peg  BOOLEAN     NOT NULL,

  -- The age of the worst leg in the whole run, never the average. A run whose BTC price is
  -- a second old and whose USDC price is an hour old is an hour old, and averaging would
  -- report the most important weakness in the answer as a rounding difference.
  -- NULL only when the run priced nothing from a market at all.
  oldest_observed_at TIMESTAMPTZ,

  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The reader always wants the newest run, so the index is ordered for exactly that.
CREATE INDEX valuation_runs_as_of_desc ON valuation_runs (as_of DESC, id DESC);

-- The audit trail: what each asset was worth, and the arithmetic that got there.
--
-- This is what makes GET /positions/{id}/lineage able to answer "which prices produced
-- this number, from which source, observed when" as well as "which events produced this
-- position" -- the product claim applied to the half the ledger cannot reach.
CREATE TABLE valuation_prices (
  run_id      BIGINT         NOT NULL REFERENCES valuation_runs (id) ON DELETE CASCADE,
  asset_id    BIGINT         NOT NULL REFERENCES assets (id),
  price_usd   NUMERIC(38,18) NOT NULL CHECK (price_usd > 0),

  -- The hops, in order, each with the rate actually applied. Multiplying them reproduces
  -- price_usd exactly -- a property the engine's tests assert, and the reason a reader who
  -- does not trust the number can check it themselves.
  path        JSONB          NOT NULL,

  assumed_peg BOOLEAN        NOT NULL,
  observed_at TIMESTAMPTZ,

  PRIMARY KEY (run_id, asset_id)
);

-- The worker writes runs; it connects as plimsoll_app like everything else, so the grant is
-- the same shape as price_ticks' and carries the same caveat (00018). No DELETE: a run is
-- what a response was built on, and removing one destroys the evidence for a number
-- somebody was shown. Pruning old runs is an owner-role act, and a decision for whoever
-- first finds the table too large.
GRANT SELECT, INSERT ON valuation_runs, valuation_prices TO plimsoll_app;
GRANT USAGE, SELECT ON SEQUENCE valuation_runs_id_seq TO plimsoll_app;

-- +goose Down
DROP TABLE valuation_prices;
DROP TABLE valuation_runs;
