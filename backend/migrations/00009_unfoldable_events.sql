-- +goose Up

-- Two review findings with one root: the schema accepted events the fold could not place,
-- and the fold's reaction to them was worse than a rejected insert.

-- 1. POSITION_ADJUSTMENT was storable and unfoldable at once. The projector errors on it,
-- the error rolls the whole transaction back, and the ledger is append-only -- so the row
-- stays forever and every later event for that integration becomes unprojectable too,
-- including through Rebuild. One row bricks the integration permanently.
--
-- The schema must not accept what the engine cannot fold. ADL and settlement come back to
-- this list in M5, together with the rule that folds them.
-- NOT VALID, and this is the general rule for this table rather than a convenience here:
-- a CHECK added to an append-only ledger can never be satisfied retroactively, because
-- the rows that violate it cannot be corrected or removed (L2). NOT VALID enforces it on
-- every future insert and skips the scan of history, which is the only honest option --
-- the alternative is a migration that cannot be applied to any database that has run.
ALTER TABLE ledger_events DROP CONSTRAINT ledger_events_event_type_check;
ALTER TABLE ledger_events ADD CONSTRAINT ledger_events_event_type_check
  CHECK (event_type IN (
    'TRADE', 'TRANSFER', 'DEPOSIT', 'WITHDRAWAL', 'FUNDING_PAYMENT',
    'FEE', 'COMMISSION_REBATE', 'LIQUIDATION')) NOT VALID;

-- 2. The projector skipped every event with a NULL instrument_id, which silently dropped a
-- standalone FEE and a funding payment: their cost left the numbers with no error and
-- nothing in freshness (L11).
--
-- There is no account-level projection for them to land on yet. Accepting money we then
-- lose is the worse of the two options, so they are refused at write time -- one rejected
-- insert instead of a number that is quietly wrong. When an account-level fee projection
-- exists, this constraint is what has to be relaxed deliberately.
--
-- Balance events keep their NULL: a deposit genuinely has no instrument.
ALTER TABLE ledger_events ADD CONSTRAINT value_events_name_their_instrument
  CHECK (event_type NOT IN ('FEE', 'COMMISSION_REBATE', 'FUNDING_PAYMENT')
         OR instrument_id IS NOT NULL) NOT VALID;

-- +goose Down
ALTER TABLE ledger_events DROP CONSTRAINT value_events_name_their_instrument;
ALTER TABLE ledger_events DROP CONSTRAINT ledger_events_event_type_check;
ALTER TABLE ledger_events ADD CONSTRAINT ledger_events_event_type_check
  CHECK (event_type IN (
    'TRADE', 'TRANSFER', 'DEPOSIT', 'WITHDRAWAL', 'FUNDING_PAYMENT',
    'FEE', 'COMMISSION_REBATE', 'LIQUIDATION', 'POSITION_ADJUSTMENT')) NOT VALID;
