-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- +goose StatementBegin
-- Reads the per-transaction tenant setting. The `true` argument is missing_ok: when the
-- setting was never applied this returns NULL, so a policy comparing against it yields
-- NULL (not true) and the row is filtered. A forgotten tenancy.InTx therefore returns an
-- empty result, never another account's data (K15, L12).
CREATE FUNCTION app_current_account() RETURNS uuid
LANGUAGE sql STABLE
SET search_path = pg_catalog, public
AS $$
  SELECT NULLIF(current_setting('app.account_id', true), '')::uuid
$$;
-- +goose StatementEnd

CREATE TABLE accounts (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email         TEXT        NOT NULL UNIQUE,
  password_hash TEXT        NOT NULL,
  is_admin      BOOLEAN     NOT NULL DEFAULT false,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  disabled_at   TIMESTAMPTZ
);

CREATE TABLE sessions (
  token_hash   BYTEA       PRIMARY KEY,
  account_id   UUID        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ NOT NULL,
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX sessions_account_id_idx ON sessions (account_id);

-- invites deliberately carry no account_id: an invite exists before the account does, so
-- it is not tenant data and L12 does not apply to it. It is protected instead by holding
-- no privileges for plimsoll_app at all -- see 00002, where the only path to this table
-- is a SECURITY DEFINER function.
CREATE TABLE invites (
  token_hash  BYTEA       PRIMARY KEY,
  email       TEXT        NOT NULL,
  created_by  UUID        REFERENCES accounts (id),
  consumed_by UUID        REFERENCES accounts (id),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ NOT NULL
);

ALTER TABLE accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts FORCE  ROW LEVEL SECURITY;
ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE  ROW LEVEL SECURITY;

CREATE POLICY accounts_self ON accounts
  FOR SELECT USING (id = app_current_account());

CREATE POLICY accounts_update_self ON accounts
  FOR UPDATE USING (id = app_current_account())
         WITH CHECK (id = app_current_account());

-- An account is created before any account context exists, so this policy cannot compare
-- against app_current_account() the way the others do. It is still tight: creation is
-- allowed only from an unbound transaction (the bootstrap case), and plimsoll_app holds
-- no INSERT privilege on this table regardless -- the only caller is auth_consume_invite,
-- which runs as the owner.
CREATE POLICY accounts_insert ON accounts
  FOR INSERT WITH CHECK (app_current_account() IS NULL OR id = app_current_account());

CREATE POLICY sessions_own ON sessions
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

-- No INSERT or DELETE on accounts: an account is created only through the invite flow,
-- and nothing in V1 deletes one.
GRANT SELECT, UPDATE ON accounts TO plimsoll_app;
GRANT SELECT, DELETE ON sessions TO plimsoll_app;
-- invites: no grant at all.

-- +goose Down
DROP TABLE invites;
DROP TABLE sessions;
DROP TABLE accounts;
DROP FUNCTION app_current_account();
