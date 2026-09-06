-- +goose Up

-- A deposit could say how much, but not of what.
--
-- ledger_events carried instrument_id and nothing else, and 00009 recorded the reasoning
-- that a balance event "genuinely has no instrument" -- which is true, and left the gap
-- open: an instrument is a pair you trade (BTC-USDT), while a deposit moves one asset.
-- Until now the coin lived only in the raw payload, so a DEPOSIT stored quantity 0.5 with
-- nothing in a typed column naming BNB.
--
-- That breaks L3. A projection is a pure fold over the ledger's columns; one that had to
-- reach into raw JSONB to learn which asset moved would be parsing exchange payloads at
-- fold time, which is the normalizer's job and the reason raw is insurance rather than a
-- data source.
--
-- Discovered while writing M2 task 5, when NormalizeDeposit had nowhere to put the coin.
ALTER TABLE ledger_events ADD COLUMN asset_id BIGINT REFERENCES assets (id);

-- A trade keeps its NULL: the instrument already names both assets, and duplicating one of
-- them here would create a second place for them to disagree.
--
-- NOT VALID for the same reason 00009's constraints are: it binds every future write
-- immediately without rewriting a table whose existing rows predate the column. The rows
-- that exist now have no asset to backfill from except raw, and guessing one is exactly
-- what this column exists to stop.
ALTER TABLE ledger_events ADD CONSTRAINT balance_events_name_their_asset
  CHECK (event_type NOT IN ('DEPOSIT', 'WITHDRAWAL', 'TRANSFER')
         OR asset_id IS NOT NULL) NOT VALID;

-- The fold reads it, so it has to be selectable. No new grant is needed on assets: the app
-- role already reads the registry and still cannot write it (00004).

-- +goose Down
ALTER TABLE ledger_events DROP CONSTRAINT balance_events_name_their_asset;
ALTER TABLE ledger_events DROP COLUMN asset_id;
