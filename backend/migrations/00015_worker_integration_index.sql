-- +goose Up

-- The worker has to know which integrations to run before it knows whose they are, and RLS
-- answers every cross-account question with an empty result.
--
-- 00003 already met this and stated the rule: anything reachable BEFORE authentication is
-- protected by privilege, anything reachable after it is protected by RLS. A SECURITY
-- DEFINER function over integrations would not work here either -- FORCE ROW LEVEL SECURITY
-- binds the owner too, so the function would run as the owner and still see nothing.
--
-- So integrations keeps FORCE ROW LEVEL SECURITY and L12 needs no exception, and this table
-- is the lookup index, exactly as account_credentials is for login. It carries no policy
-- because it is not tenant data -- it exists to answer a question asked before any tenant is
-- known -- and it is protected instead by plimsoll_app holding no grant on it at all.
CREATE TABLE worker_integrations (
  integration_id UUID    PRIMARY KEY,
  account_id     UUID    NOT NULL,

  -- Whether there is anything to run. An integration with no credential cannot be ingested
  -- from, and claiming its lease would keep another worker from a job neither can do.
  runnable       BOOLEAN NOT NULL
);
CREATE INDEX worker_integrations_runnable_idx ON worker_integrations (runnable);

-- +goose StatementBegin
-- Maintained by trigger rather than by the connection flow. An index that a future writer
-- can forget to update is an index that will be wrong, and being wrong here means an
-- account whose trades are never ingested and nothing that says so.
CREATE FUNCTION worker_integrations_sync() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    DELETE FROM worker_integrations WHERE integration_id = OLD.id;
    RETURN OLD;
  END IF;

  INSERT INTO worker_integrations (integration_id, account_id, runnable)
  VALUES (NEW.id, NEW.account_id,
          NEW.status = 'active' AND NEW.credential_ciphertext IS NOT NULL)
  ON CONFLICT (integration_id) DO UPDATE SET
    account_id = EXCLUDED.account_id,
    runnable   = EXCLUDED.runnable;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER integrations_maintain_worker_index
AFTER INSERT OR UPDATE OR DELETE ON integrations
FOR EACH ROW EXECUTE FUNCTION worker_integrations_sync();

-- Existing rows. The read has to see past the table's own FORCE, so it is lifted for the
-- length of this statement and put back immediately -- visible, contained, and in a
-- migration that runs as the owner rather than anywhere the application can reach.
ALTER TABLE integrations NO FORCE ROW LEVEL SECURITY;
INSERT INTO worker_integrations (integration_id, account_id, runnable)
SELECT id, account_id, status = 'active' AND credential_ciphertext IS NOT NULL
FROM integrations
ON CONFLICT (integration_id) DO NOTHING;
ALTER TABLE integrations FORCE ROW LEVEL SECURITY;

-- +goose StatementBegin
-- The worker's only cross-account read, and it returns ids and nothing else: not the label,
-- not the exchange, and certainly not the credential. The whole question it answers is
-- "what should I try to claim"; everything after that is bound to one account.
--
-- SETOF a real table rather than an ad-hoc RETURNS TABLE, for the reason 00003 records:
-- sqlc cannot infer the column types of a set-returning function, but it knows this table's.
CREATE FUNCTION worker_active_integrations()
RETURNS SETOF worker_integrations
LANGUAGE sql SECURITY DEFINER STABLE
SET search_path = pg_catalog, public
AS $$
  SELECT w.* FROM worker_integrations w
  WHERE w.runnable
  ORDER BY w.account_id, w.integration_id
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION worker_active_integrations() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION worker_active_integrations() TO plimsoll_app;

-- +goose Down
DROP FUNCTION worker_active_integrations();
DROP TRIGGER integrations_maintain_worker_index ON integrations;
DROP FUNCTION worker_integrations_sync();
DROP TABLE worker_integrations;
