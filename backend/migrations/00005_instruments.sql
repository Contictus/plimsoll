-- +goose Up

-- Instruments are reference data on the same terms as assets (00004): shared across
-- accounts, no account_id, no RLS, readable but not writable by plimsoll_app.
--
-- An asset is the thing (BTC); an instrument is the thing you trade (BTC-USDT-PERP). K10
-- separates them because most valuation errors are identity errors, not price errors.

CREATE TABLE instruments (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  canonical_symbol TEXT           NOT NULL UNIQUE,
  kind             TEXT           NOT NULL CHECK (kind IN ('spot', 'perp')),
  base_asset_id    BIGINT         NOT NULL REFERENCES assets (id),
  quote_asset_id   BIGINT         NOT NULL REFERENCES assets (id),

  -- What the contract settles in. Null for spot, required for a perp: a coin-margined
  -- contract settles in the base asset and a USDT-margined one in the quote, and valuing
  -- the first as if it were the second is a whole-position error, not a rounding one.
  settle_asset_id  BIGINT         REFERENCES assets (id),

  contract_size    NUMERIC(38,18) NOT NULL DEFAULT 1,
  created_at       TIMESTAMPTZ    NOT NULL DEFAULT now(),

  CONSTRAINT perps_settle_somewhere
    CHECK (kind <> 'perp' OR settle_asset_id IS NOT NULL),
  CONSTRAINT instrument_legs_differ
    CHECK (base_asset_id <> quote_asset_id)
);

CREATE TABLE instrument_aliases (
  id              BIGINT    GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  exchange        TEXT      NOT NULL,

  -- Part of the key, not a detail. On Binance, spot BTCUSDT and perpetual BTCUSDT are the
  -- same string and different instruments; omitting market merges the two positions and
  -- every downstream number drifts while looking plausible (K10).
  market          TEXT      NOT NULL CHECK (market IN ('spot', 'usdm', 'coinm')),

  exchange_symbol TEXT      NOT NULL,
  instrument_id   BIGINT    NOT NULL REFERENCES instruments (id) ON DELETE CASCADE,

  -- Half-open [valid_from, valid_to), as in 00004.
  validity        TSTZRANGE NOT NULL,

  CONSTRAINT instrument_aliases_no_overlapping_windows
    EXCLUDE USING gist (
      exchange        WITH =,
      market          WITH =,
      exchange_symbol WITH =,
      validity        WITH &&
    )
);

CREATE INDEX instrument_aliases_resolution_idx
  ON instrument_aliases USING gist (exchange, market, exchange_symbol, validity);

GRANT SELECT ON instruments, instrument_aliases TO plimsoll_app;

-- +goose Down
DROP TABLE instrument_aliases;
DROP TABLE instruments;
