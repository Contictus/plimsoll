-- +goose Up

-- Envelope-encrypted exchange credentials (K25). The three columns travel together: the
-- ciphertext is opened by the data key in wrapped_dek, which is itself opened by the
-- master key named by key_version. Keeping the version on the row is what makes a
-- rotation additive -- old rows stay readable while new ones are written under the new
-- key, and no migration has to touch ciphertext.
--
-- All nullable, because an integration exists before its credential does: M2 creates the
-- row, then verifies the key, then stores it.
ALTER TABLE integrations
  ADD COLUMN credential_ciphertext  BYTEA,
  ADD COLUMN wrapped_dek            BYTEA,
  ADD COLUMN key_version            INT,
  ADD COLUMN credential_verified_at TIMESTAMPTZ;

-- A half-written credential is unopenable and indistinguishable from an absent one, so
-- the three columns are constrained to arrive and depart together. Enforced here rather
-- than in Go because the store is the last place that can still refuse the row.
ALTER TABLE integrations
  ADD CONSTRAINT credential_columns_travel_together CHECK (
    (credential_ciphertext IS NULL AND wrapped_dek IS NULL AND key_version IS NULL)
    OR
    (credential_ciphertext IS NOT NULL AND wrapped_dek IS NOT NULL AND key_version IS NOT NULL)
  );

-- A key version is assigned by the provider and starts at 1; zero or negative would mean
-- the writer supplied one of its own.
ALTER TABLE integrations
  ADD CONSTRAINT key_version_is_positive CHECK (key_version IS NULL OR key_version > 0);

-- M1 granted SELECT only, deliberately: a table with no write path cannot be written to
-- by a bug. This is the milestone that earns the write -- the connection flow creates an
-- integration and stores its credential, both as the app role, both under RLS.
GRANT INSERT, UPDATE ON integrations TO plimsoll_app;

-- +goose Down
REVOKE INSERT, UPDATE ON integrations FROM plimsoll_app;

ALTER TABLE integrations
  DROP CONSTRAINT key_version_is_positive,
  DROP CONSTRAINT credential_columns_travel_together,
  DROP COLUMN credential_verified_at,
  DROP COLUMN key_version,
  DROP COLUMN wrapped_dek,
  DROP COLUMN credential_ciphertext;
