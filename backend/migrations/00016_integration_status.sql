-- +goose Up

-- What the worker is doing, written where a reader can find it.
--
-- The supervisor already knows its state and already turns it into a freshness reason --
-- in the worker process's memory. The API is a different process, and on a fleet a
-- different machine, so it cannot ask. Without this table a portfolio response has no way
-- to say "the live feed for this integration is down", and would answer with numbers that
-- look current because nothing contradicted them. That is the exact failure L11 exists to
-- reject, so the state is published rather than inferred (K39).
--
-- This is not a projection and not a source of truth: it is one writer's report about
-- itself. Losing every row costs the reader its freshness detail and nothing else -- and
-- the reader treats a missing row as "nobody is ingesting this", which is the safe
-- reading of silence.
CREATE TABLE integration_status (
  account_id     UUID        NOT NULL,
  integration_id UUID        NOT NULL PRIMARY KEY,

  -- The worker.State enum. Spelled out here as well as in Go on purpose: a typo'd state
  -- is a freshness reason no client can match on, and the constraint turns that into a
  -- rejected write instead of a wrong dashboard.
  state          TEXT        NOT NULL
                   CHECK (state IN ('connecting', 'live', 'degraded',
                                    'resyncing', 'backfilling')),

  -- Which worker process is speaking. Not for display: it is what makes two workers
  -- disagreeing visible in the data rather than only in the logs.
  owner_id       TEXT        NOT NULL CHECK (owner_id <> ''),

  -- When this state began, so freshness can say how long it has been that way.
  since          TIMESTAMPTZ NOT NULL,

  -- When the worker last said anything. A reader that finds this older than the lease TTL
  -- knows the report itself has stopped, which is worse than any state it names.
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

  FOREIGN KEY (account_id, integration_id)
    REFERENCES integrations (account_id, id) ON DELETE CASCADE
);

ALTER TABLE integration_status ENABLE ROW LEVEL SECURITY;
ALTER TABLE integration_status FORCE  ROW LEVEL SECURITY;

CREATE POLICY integration_status_own ON integration_status
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

GRANT SELECT, INSERT, UPDATE, DELETE ON integration_status TO plimsoll_app;

-- +goose Down
DROP TABLE integration_status;
