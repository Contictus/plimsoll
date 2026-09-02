-- +goose Up

-- The asset registry is REFERENCE DATA, not tenant data: BTC is BTC for every account.
-- It therefore carries no account_id and no RLS, which is a deliberate exception to the
-- shape L12 describes rather than an oversight. What protects it instead is privilege --
-- plimsoll_app may read it and may not write it, so a bug in the ingest path cannot
-- invent an asset to make an unknown symbol resolve. Curation is an owner-role act, the
-- same way migrations are.
--
-- The alternative, per-account registries, would mean two accounts trading the same coin
-- could disagree about what it is, and reconciliation between them would be meaningless.

-- btree_gist lets an exclusion constraint mix equality on plain columns with overlap on a
-- range, which is what makes "one alias per symbol per instant" enforceable.
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE assets (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  canonical_symbol    TEXT        NOT NULL UNIQUE,
  kind                TEXT        NOT NULL
                        CHECK (kind IN ('native', 'token', 'stablecoin', 'fiat')),
  chain               TEXT,
  contract_address    TEXT,
  is_wrapped          BOOLEAN     NOT NULL DEFAULT false,
  underlying_asset_id BIGINT      REFERENCES assets (id),
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- A wrapped asset that does not name its underlying is exactly the identity error K10
  -- exists to prevent: WBTC valued as if it had nothing to do with BTC.
  CONSTRAINT wrapped_assets_name_their_underlying
    CHECK (NOT is_wrapped OR underlying_asset_id IS NOT NULL),

  -- A token without a chain is unidentifiable: the same contract address exists on
  -- several chains and the same ticker on all of them.
  CONSTRAINT tokens_carry_a_chain
    CHECK (kind <> 'token' OR chain IS NOT NULL)
);

CREATE TABLE asset_aliases (
  id              BIGINT    GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  source          TEXT      NOT NULL,   -- binance | bybit | coingecko
  external_symbol TEXT      NOT NULL,
  asset_id        BIGINT    NOT NULL REFERENCES assets (id) ON DELETE CASCADE,

  -- Half-open [valid_from, valid_to). An event exactly at valid_to belongs to the next
  -- window; without that rule one instant resolves two ways. An unbounded upper end is a
  -- symbol that is still listed.
  validity        TSTZRANGE NOT NULL,

  -- K22: two aliases may never claim one symbol at one instant, so resolution is a lookup
  -- and never a choice. This is what stops a bad backfill from corrupting the registry in
  -- a way that only shows up months later as a wrong position.
  CONSTRAINT asset_aliases_no_overlapping_windows
    EXCLUDE USING gist (
      source          WITH =,
      external_symbol WITH =,
      validity        WITH &&
    )
);

-- The resolution index. The exclusion constraint's gist index cannot serve this lookup
-- shape, so the containment query gets its own.
CREATE INDEX asset_aliases_resolution_idx
  ON asset_aliases USING gist (source, external_symbol, validity);

GRANT SELECT ON assets, asset_aliases TO plimsoll_app;

-- +goose Down
DROP TABLE asset_aliases;
DROP TABLE assets;
