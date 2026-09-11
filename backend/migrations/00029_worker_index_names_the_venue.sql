-- +goose Up

-- The worker index says WHICH venue an integration is.
--
-- Until M8 there was one, so the worker could assemble a Binance client for every assignment
-- and be right. With a second venue the assignment has to carry the answer: a Bybit
-- integration handed a Binance client authenticates nothing, and the failure arrives as a
-- signature rejection rather than as anything that names the real mistake (B1).
--
-- Defaulted to 'binance' rather than left NULL: every row that exists is one, and a nullable
-- column would make "we do not know" a state the dispatch has to handle forever.
ALTER TABLE worker_integrations ADD COLUMN exchange TEXT NOT NULL DEFAULT 'binance';

-- +goose StatementBegin
-- The trigger writes it from now on. Maintained by trigger rather than by the connection flow
-- for the reason 00015 gave: an index a future writer can forget to update is an index that
-- will be wrong, and being wrong here means an account whose history is never ingested and
-- nothing that says so.
CREATE OR REPLACE FUNCTION worker_integrations_sync() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    DELETE FROM worker_integrations WHERE integration_id = OLD.id;
    RETURN OLD;
  END IF;

  INSERT INTO worker_integrations (integration_id, account_id, exchange, runnable)
  VALUES (NEW.id, NEW.account_id, NEW.exchange,
          NEW.status = 'active' AND NEW.credential_ciphertext IS NOT NULL)
  ON CONFLICT (integration_id) DO UPDATE SET
    account_id = EXCLUDED.account_id,
    exchange   = EXCLUDED.exchange,
    runnable   = EXCLUDED.runnable;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Existing rows get their real venue. The read has to see past the table's own FORCE, so it is
-- lifted for the length of this statement and put back immediately -- the same contained,
-- visible shape 00015 used, in a migration that runs as the owner.
ALTER TABLE integrations NO FORCE ROW LEVEL SECURITY;
UPDATE worker_integrations w
   SET exchange = i.exchange
  FROM integrations i
 WHERE i.id = w.integration_id;
ALTER TABLE integrations FORCE ROW LEVEL SECURITY;

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION worker_integrations_sync() RETURNS trigger
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
ALTER TABLE worker_integrations DROP COLUMN exchange;
