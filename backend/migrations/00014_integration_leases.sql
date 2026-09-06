-- +goose Up

-- One writer per integration, enforced by the database rather than by convention.
--
-- L6 says a projection advances per integration_id, protected by a single-writer lease.
-- Until now there was one writer -- the backfill -- and the lease was a design note. The
-- live stream makes it two, and two writers on one integration means two folds racing on
-- one cursor: the loser's events land behind the winner's cursor, are never read, and the
-- position stays permanently wrong with nothing in freshness to say so (00010 exists
-- because of exactly that shape).
--
-- Not in Redis. A lost lease table would let two workers write at once, and K28 gives Redis
-- only what can be rebuilt (L14). This cannot be rebuilt: it is the thing that decides who
-- is allowed to write.
CREATE TABLE integration_leases (
  -- One row per integration is the invariant, so it is the primary key rather than an
  -- index over a table that could hold two.
  integration_id UUID        PRIMARY KEY,
  account_id     UUID        NOT NULL,

  -- Which worker process holds it. Opaque to the schema: a process-unique string, minted
  -- per start, never a hostname -- two processes on one host would then share an identity
  -- and each would believe it held the other's lease.
  owner_id       TEXT        NOT NULL,

  acquired_at    TIMESTAMPTZ NOT NULL,

  -- The clock that matters. Both written and compared with the database's now(), never a
  -- worker's: two workers with skewed clocks would disagree about whether a lease had
  -- expired, and the one running fast would claim a lease the other still held. One clock
  -- is what makes the claim decidable at all.
  expires_at     TIMESTAMPTZ NOT NULL,

  FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id),

  CONSTRAINT lease_expires_after_it_is_acquired CHECK (expires_at > acquired_at),
  CONSTRAINT lease_owner_is_named CHECK (owner_id <> '')
);

ALTER TABLE integration_leases ENABLE ROW LEVEL SECURITY;
ALTER TABLE integration_leases FORCE  ROW LEVEL SECURITY;

CREATE POLICY integration_leases_own ON integration_leases
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

GRANT SELECT, INSERT, UPDATE, DELETE ON integration_leases TO plimsoll_app;

-- +goose Down
DROP TABLE integration_leases;
