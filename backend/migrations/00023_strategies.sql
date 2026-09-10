-- +goose Up

-- The strategies registry M1 promised.
--
-- `position_strategies` has existed since 00007, deliberately, because where the tag lives is
-- a schema decision and those are the expensive ones to defer (K30): the tag is user input,
-- and `positions` is a projection that every rebuild drops. What 00007 could not add was the
-- foreign key, because the thing it points at is this table -- the sleeve model K13 places in
-- M6. It arrives here, and the column stops being a uuid that means whatever wrote it.
--
-- Why any of this exists: a delta-neutral basis trade -- spot long plus perp short -- reads
-- as "2x leveraged, risky" when its net delta is about zero, and a system that cannot group
-- the two legs alerts constantly and incorrectly until the user silences the channel. At that
-- point the alerting has negative value.
CREATE TABLE strategies (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id UUID        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  name       TEXT        NOT NULL,

  -- What kind of exposure the group is meant to have. Not decoration: a basis trade whose net
  -- delta is far from zero is a broken hedge, and only the declared intent makes that
  -- checkable. The vocabulary is closed so a new kind has to be named here, where the risk
  -- engine's reader can see it.
  kind       TEXT        NOT NULL DEFAULT 'directional'
             CHECK (kind IN ('directional', 'basis', 'market_neutral', 'other')),

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- The name is what the user types and what an alert says. Two of them would make an alert
  -- ambiguous about which book it came from.
  CONSTRAINT strategies_named_once_per_account UNIQUE (account_id, name),

  -- Exists so the tag below can reference (account_id, id), which makes assigning a position
  -- to another account's strategy impossible at the storage layer rather than merely
  -- unlikely -- the same shape as the ledger's composite key (K29).
  CONSTRAINT strategies_account_scoped_id UNIQUE (account_id, id)
);

ALTER TABLE position_strategies
  ADD CONSTRAINT position_strategies_strategy_fkey
    FOREIGN KEY (account_id, strategy_id) REFERENCES strategies (account_id, id)
    ON DELETE CASCADE;

-- Aggregating by strategy is the read this whole table exists for.
CREATE INDEX position_strategies_by_strategy_idx
  ON position_strategies (account_id, strategy_id);

ALTER TABLE strategies ENABLE ROW LEVEL SECURITY;
ALTER TABLE strategies FORCE  ROW LEVEL SECURITY;

CREATE POLICY strategies_own ON strategies
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

GRANT SELECT, INSERT, UPDATE, DELETE ON strategies TO plimsoll_app;

-- +goose Down
DROP INDEX position_strategies_by_strategy_idx;
ALTER TABLE position_strategies DROP CONSTRAINT position_strategies_strategy_fkey;
DROP TABLE strategies;
