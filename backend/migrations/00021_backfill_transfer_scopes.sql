-- +goose Up

-- The scope vocabulary is closed on purpose (00013): a typo'd scope is a walk that silently
-- restarts from nothing every run, which looks exactly like a working backfill that never
-- finishes. So admitting a new kind of walk is a migration, and this is that migration.
--
-- One scope per transfer DIRECTION rather than one for transfers. `type` is a required
-- parameter on the universal-transfer endpoint and is a direction rather than a category --
-- MAIN_UMFUTURE and UMFUTURE_MAIN are two separate queries (F10) -- so eight walks, eight
-- cursors, and an import interrupted in the sixth resumes there instead of re-reading the
-- five that finished.
--
-- The pattern admits the shape rather than the eight names. Naming them here would put the
-- venue's vocabulary in two places, and the place that is harder to change would win an
-- argument it should not be in: the walked set is the normalizer's (WalkedTransferTypes),
-- and this constraint's job is to catch a typo, not to ratify a list.
ALTER TABLE backfill_progress DROP CONSTRAINT backfill_scope_is_known;

ALTER TABLE backfill_progress ADD CONSTRAINT backfill_scope_is_known CHECK (
  scope IN ('discover', 'deposits', 'withdrawals')
  OR scope ~ '^trades:[A-Z0-9]+$'
  OR scope ~ '^transfers:[A-Z0-9_]+$');

-- +goose Down
ALTER TABLE backfill_progress DROP CONSTRAINT backfill_scope_is_known;

ALTER TABLE backfill_progress ADD CONSTRAINT backfill_scope_is_known CHECK (
  scope IN ('discover', 'deposits', 'withdrawals')
  OR scope ~ '^trades:[A-Z0-9]+$');
