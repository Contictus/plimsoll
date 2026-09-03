-- +goose Up

-- Two corrections from an independent review of M1.

-- 1. The canonical order (L7) is compared in two places: Go's byte-wise `>` in the
-- engine's cursor, and the database collation in ORDER BY, in the keyset comparison and
-- in the index behind it. Those agree only while these columns collate like C.
--
-- The database is created without an explicit locale and inherits en_US.utf8 from
-- template1. On the musl-based alpine image that behaves like C, so the two orders happen
-- to agree today. On a glibc Postgres -- the standard Debian image, or any managed
-- instance -- en_US.UTF-8 ignores punctuation at the primary level, and venue_event_id is
-- built entirely out of colon-separated parts. The orders would diverge, the fold would
-- receive two tied-timestamp events in an order Apply rejects as out of order, and the
-- projection would halt. Declaring the collation removes the dependency on where this
-- happens to be deployed.
ALTER TABLE ledger_events      ALTER COLUMN venue_event_id      TYPE TEXT COLLATE "C";
ALTER TABLE positions          ALTER COLUMN last_venue_event_id TYPE TEXT COLLATE "C";
ALTER TABLE projection_cursors ALTER COLUMN last_venue_event_id TYPE TEXT COLLATE "C";

-- Ordered by SQL in ListPositionFees and sorted byte-wise by addFee, for the same reason.
ALTER TABLE position_fees      ALTER COLUMN fee_asset           TYPE TEXT COLLATE "C";

-- 2. The projection tables referenced integrations with no ON DELETE action, so they
-- blocked deletion of an integration -- and, through integrations' own cascade, of an
-- account. 00002_accounts_delete_policy.sql was added specifically to keep that path open
-- for an operator and for fixture teardown, and 00007 closed it again by accident.
--
-- CASCADE is the correct action precisely because these are projections: dropping them is
-- always safe, since they can be rebuilt from the ledger (L3). ledger_events keeps
-- NO ACTION, and must: an event is not droppable, and a foreign key that would delete one
-- is a foreign key that violates L2.
ALTER TABLE positions
  DROP CONSTRAINT positions_account_id_integration_id_fkey,
  ADD CONSTRAINT positions_account_id_integration_id_fkey
    FOREIGN KEY (account_id, integration_id)
    REFERENCES integrations (account_id, id) ON DELETE CASCADE;

ALTER TABLE projection_cursors
  DROP CONSTRAINT projection_cursors_account_id_integration_id_fkey,
  ADD CONSTRAINT projection_cursors_account_id_integration_id_fkey
    FOREIGN KEY (account_id, integration_id)
    REFERENCES integrations (account_id, id) ON DELETE CASCADE;

ALTER TABLE position_strategies
  DROP CONSTRAINT position_strategies_account_id_integration_id_fkey,
  ADD CONSTRAINT position_strategies_account_id_integration_id_fkey
    FOREIGN KEY (account_id, integration_id)
    REFERENCES integrations (account_id, id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE position_strategies
  DROP CONSTRAINT position_strategies_account_id_integration_id_fkey,
  ADD CONSTRAINT position_strategies_account_id_integration_id_fkey
    FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id);
ALTER TABLE projection_cursors
  DROP CONSTRAINT projection_cursors_account_id_integration_id_fkey,
  ADD CONSTRAINT projection_cursors_account_id_integration_id_fkey
    FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id);
ALTER TABLE positions
  DROP CONSTRAINT positions_account_id_integration_id_fkey,
  ADD CONSTRAINT positions_account_id_integration_id_fkey
    FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id);

ALTER TABLE position_fees      ALTER COLUMN fee_asset           TYPE TEXT;
ALTER TABLE projection_cursors ALTER COLUMN last_venue_event_id TYPE TEXT;
ALTER TABLE positions          ALTER COLUMN last_venue_event_id TYPE TEXT;
ALTER TABLE ledger_events      ALTER COLUMN venue_event_id      TYPE TEXT;
