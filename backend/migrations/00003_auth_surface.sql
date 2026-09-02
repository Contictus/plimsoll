-- +goose Up

-- The rule this migration establishes, and the one to hold to as the schema grows:
--
--   Anything reachable BEFORE authentication is protected by privilege.
--   Anything reachable AFTER authentication is protected by RLS.
--
-- Login has to find an account by email, which is exactly the thing it does not know the
-- account id for -- and accounts is under FORCE ROW LEVEL SECURITY, which binds the owner
-- too, so even a SECURITY DEFINER function cannot read it without an account context.
-- account_credentials is that lookup index. It is not tenant data (it exists to answer a
-- question asked before any tenant is known), so it carries no account_id policy; it is
-- protected instead by plimsoll_app holding no grant on it at all, the same way invites
-- is. accounts therefore keeps FORCE ROW LEVEL SECURITY, and L12 needs no exception.
CREATE TABLE account_credentials (
  email         TEXT        PRIMARY KEY,
  account_id    UUID        NOT NULL UNIQUE REFERENCES accounts (id) ON DELETE CASCADE,
  password_hash TEXT        NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The hash now lives in exactly one place. Keeping a second copy on accounts would put a
-- credential inside the table the application reads on every request.
ALTER TABLE accounts DROP COLUMN password_hash;

-- accounts.email stays: it is the user's own identity, read back by GET /me under RLS.
-- It is written once, together with the credential row, inside auth_consume_invite.

-- +goose StatementBegin
-- Returns nothing for an unknown email. The caller cannot distinguish that from a wrong
-- password, which is what keeps login from being a user-enumeration oracle.
--
-- disabled_at is deliberately NOT checked here: that is a policy decision, and it belongs
-- in Go where it is testable, after the account id is known and a tenancy.InTx read is
-- possible.
-- SETOF account_credentials rather than an ad-hoc RETURNS TABLE: sqlc cannot infer the
-- column types of a set-returning function, but it already knows this table's shape, so
-- the generated code comes back typed instead of as interface{}.
CREATE FUNCTION auth_lookup_credentials(p_email TEXT)
RETURNS SETOF account_credentials
LANGUAGE sql SECURITY DEFINER STABLE
SET search_path = pg_catalog, public
AS $$
  SELECT c.* FROM account_credentials c WHERE c.email = p_email
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- p_account_id is the unverified claim carried in the token prefix. Binding it first is
-- what makes the sessions read possible at all under FORCE ROW LEVEL SECURITY; the row
-- lookup on token_hash is what verifies the claim. A forged prefix binds an account whose
-- policy then hides every row, so it returns no rows.
CREATE FUNCTION auth_resolve_session(
  p_account_id UUID,
  p_token_hash BYTEA,
  p_now        TIMESTAMPTZ
) RETURNS SETOF UUID
LANGUAGE plpgsql SECURITY DEFINER VOLATILE
SET search_path = pg_catalog, public
AS $$
BEGIN
  PERFORM set_config('app.account_id', p_account_id::text, true);

  RETURN QUERY
  UPDATE sessions
     SET last_seen_at = p_now
   WHERE token_hash = p_token_hash
     AND expires_at > p_now
  RETURNING sessions.account_id;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Consumes an invite and creates the account, its email and its credential row in one
-- statement chain, so a crash between them cannot leave a consumed invite with no account
-- behind it.
CREATE FUNCTION auth_consume_invite(
  p_token_hash    BYTEA,
  p_email         TEXT,
  p_password_hash TEXT,
  p_now           TIMESTAMPTZ
) RETURNS SETOF UUID
LANGUAGE plpgsql SECURITY DEFINER VOLATILE
SET search_path = pg_catalog, public
AS $$
DECLARE
  v_account_id UUID;
BEGIN
  -- FOR UPDATE serializes two concurrent redemptions of the same link: the second waits,
  -- then sees consumed_by already set and finds no row.
  PERFORM 1 FROM invites
   WHERE token_hash = p_token_hash
     AND consumed_by IS NULL
     AND expires_at > p_now
     AND email = p_email
     FOR UPDATE;
  IF NOT FOUND THEN
    -- No row rather than NULL: the caller sees pgx.ErrNoRows, which is unambiguous.
    RETURN;
  END IF;

  -- The id is generated up front so the account context can be bound before the INSERT;
  -- accounts is under FORCE ROW LEVEL SECURITY and its insert policy admits either an
  -- unbound transaction or the account's own id.
  v_account_id := gen_random_uuid();
  PERFORM set_config('app.account_id', v_account_id::text, true);

  INSERT INTO accounts (id, email) VALUES (v_account_id, p_email);
  INSERT INTO account_credentials (email, account_id, password_hash)
  VALUES (p_email, v_account_id, p_password_hash);

  UPDATE invites SET consumed_by = v_account_id WHERE token_hash = p_token_hash;
  RETURN NEXT v_account_id;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION auth_create_session(
  p_account_id UUID,
  p_token_hash BYTEA,
  p_expires_at TIMESTAMPTZ
) RETURNS VOID
LANGUAGE plpgsql SECURITY DEFINER VOLATILE
SET search_path = pg_catalog, public
AS $$
BEGIN
  PERFORM set_config('app.account_id', p_account_id::text, true);
  INSERT INTO sessions (token_hash, account_id, expires_at)
  VALUES (p_token_hash, p_account_id, p_expires_at);
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION auth_delete_session(p_account_id UUID, p_token_hash BYTEA)
RETURNS VOID
LANGUAGE plpgsql SECURITY DEFINER VOLATILE
SET search_path = pg_catalog, public
AS $$
BEGIN
  PERFORM set_config('app.account_id', p_account_id::text, true);
  DELETE FROM sessions WHERE token_hash = p_token_hash;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION
  auth_lookup_credentials(TEXT),
  auth_resolve_session(UUID, BYTEA, TIMESTAMPTZ),
  auth_consume_invite(BYTEA, TEXT, TEXT, TIMESTAMPTZ),
  auth_create_session(UUID, BYTEA, TIMESTAMPTZ),
  auth_delete_session(UUID, BYTEA)
FROM PUBLIC;

GRANT EXECUTE ON FUNCTION
  auth_lookup_credentials(TEXT),
  auth_resolve_session(UUID, BYTEA, TIMESTAMPTZ),
  auth_consume_invite(BYTEA, TEXT, TEXT, TIMESTAMPTZ),
  auth_create_session(UUID, BYTEA, TIMESTAMPTZ),
  auth_delete_session(UUID, BYTEA)
TO plimsoll_app;

-- account_credentials: no grant to plimsoll_app, deliberately. The five functions above
-- are the entire path.

-- +goose Down
DROP FUNCTION auth_delete_session(UUID, BYTEA);
DROP FUNCTION auth_create_session(UUID, BYTEA, TIMESTAMPTZ);
DROP FUNCTION auth_consume_invite(BYTEA, TEXT, TEXT, TIMESTAMPTZ);
DROP FUNCTION auth_resolve_session(UUID, BYTEA, TIMESTAMPTZ);
DROP FUNCTION auth_lookup_credentials(TEXT);
ALTER TABLE accounts ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';
DROP TABLE account_credentials;
