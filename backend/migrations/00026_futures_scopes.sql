-- +goose Up

-- The USD-M walks. As in 00021, admitting a new kind of walk is a migration: the scope
-- vocabulary is closed because a typo'd scope is a walk that silently restarts from nothing
-- every run, which looks exactly like a working backfill that never finishes.
--
-- Two shapes. `usdm:income` is one walk for the whole account, because the income endpoint
-- takes no symbol and returns every cash flow at once -- splitting it per symbol would
-- multiply the most expensive request in this system (weight 30) by the size of the book.
-- `usdm:trades:<SYMBOL>` is one per contract, because userTrades requires a symbol.
--
-- M5 shipped the futures normalizers with nothing that called them: the fills and the funding
-- payments had a fold and no walk, so the perpetual half of the ledger stayed empty while the
-- milestone read as complete. This constraint is where that gap is closed.
ALTER TABLE backfill_progress DROP CONSTRAINT backfill_scope_is_known;

ALTER TABLE backfill_progress ADD CONSTRAINT backfill_scope_is_known CHECK (
  scope IN ('discover', 'deposits', 'withdrawals', 'usdm:income')
  OR scope ~ '^trades:[A-Z0-9]+$'
  OR scope ~ '^transfers:[A-Z0-9_]+$'
  OR scope ~ '^usdm:trades:[A-Z0-9]+$');

-- +goose Down
ALTER TABLE backfill_progress DROP CONSTRAINT backfill_scope_is_known;

ALTER TABLE backfill_progress ADD CONSTRAINT backfill_scope_is_known CHECK (
  scope IN ('discover', 'deposits', 'withdrawals')
  OR scope ~ '^trades:[A-Z0-9]+$'
  OR scope ~ '^transfers:[A-Z0-9_]+$');
