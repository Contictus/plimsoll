-- +goose Up

-- Where an alert is sent, and the secret that lets it be sent.
--
-- A Telegram bot token and a webhook URL are both credentials: whoever holds one can message
-- the user as us, for as long as it lives. So they are envelope-encrypted exactly like an
-- exchange key -- per-account DEK wrapped by the master KEK (K25) -- and never returned by
-- any endpoint, never logged, never put in an error (L13).
--
-- A webhook URL is the less obvious one. It looks like configuration and it is a bearer
-- token wearing a different hat.
CREATE TABLE alert_channels (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id UUID        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,

  kind       TEXT        NOT NULL CHECK (kind IN ('telegram', 'webhook')),

  -- What the user calls it, so a listing can name a channel without revealing anything about
  -- how to reach it.
  label      TEXT        NOT NULL,
  enabled    BOOLEAN     NOT NULL DEFAULT true,

  -- The channel's secrets, sealed. Shape depends on kind and is the concern of
  -- internal/alert, not of this table: a column per field would put "which fields does a
  -- telegram channel have" in the schema, where changing it is a migration.
  config_ciphertext BYTEA NOT NULL,
  wrapped_dek       BYTEA NOT NULL,
  key_version       INT   NOT NULL,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT alert_channels_labelled_once_per_account UNIQUE (account_id, label)
);

ALTER TABLE alert_channels ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_channels FORCE  ROW LEVEL SECURITY;

CREATE POLICY alert_channels_own ON alert_channels
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

GRANT SELECT, INSERT, UPDATE, DELETE ON alert_channels TO plimsoll_app;

-- +goose Down
DROP TABLE alert_channels;
