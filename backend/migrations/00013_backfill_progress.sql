-- +goose Up

-- Where each historical walk got to, so an interrupted backfill resumes instead of
-- starting over. Restarting is not merely slow: re-probing every spot symbol costs 40-60k
-- weight, and a walk that always begins at the oldest trade never reaches the newest on an
-- account with a long history.
--
-- The cursor is advanced in the same transaction as the events it describes. A crash
-- between the two would either lose events (cursor first) or replay them forever (events
-- first), and the ledger is append-only, so neither is repairable by an UPDATE (L2).
CREATE TABLE backfill_progress (
  account_id     UUID        NOT NULL,
  integration_id UUID        NOT NULL,

  -- What is being walked. Per scope rather than one cursor per integration, because the
  -- walks have different shapes and different cursors: trades page by trade id, deposits
  -- page by time window. A single cursor would have to mean both, and finishing one walk
  -- would silently mark the other done.
  --
  -- 'withdrawals' is accepted here and has no walker yet: the withdraw status enum and the
  -- timezone of applyTime are both undocumented, so NormalizeWithdrawal does not exist
  -- (docs/BINANCE-API-NOTES.md section 5). The scope name is reserved so that adding the
  -- walker later is code rather than a migration.
  scope          TEXT        NOT NULL,

  -- Opaque to the schema and interpreted only by the walker that wrote it: a trade id for
  -- 'trades:<symbol>', an RFC 3339 window end for 'deposits', the last symbol probed for
  -- 'discover'. Empty means "nothing walked yet", which is why it is NOT NULL -- a NULL
  -- cursor and a zero cursor would be two spellings of the same state.
  cursor         TEXT        NOT NULL DEFAULT '',

  -- NULL while the walk is still incomplete. This is the column that answers "is this
  -- account's history whole?", and L11 turns a NULL here into backfill_incomplete in the
  -- response's freshness rather than into silence.
  completed_at   TIMESTAMPTZ,
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

  PRIMARY KEY (integration_id, scope),

  -- The same composite key ledger_events uses: a progress row for another account's
  -- integration is unstorable rather than merely invisible.
  FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id),

  -- A typo'd scope is a walk that silently restarts from nothing every run, which looks
  -- exactly like a working backfill that never finishes.
  CONSTRAINT backfill_scope_is_known CHECK (
    scope IN ('discover', 'deposits', 'withdrawals')
    OR scope ~ '^trades:[A-Z0-9]+$')
);

ALTER TABLE backfill_progress ENABLE ROW LEVEL SECURITY;
ALTER TABLE backfill_progress FORCE  ROW LEVEL SECURITY;

CREATE POLICY backfill_progress_own ON backfill_progress
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

GRANT SELECT, INSERT, UPDATE, DELETE ON backfill_progress TO plimsoll_app;

-- +goose Down
DROP TABLE backfill_progress;
