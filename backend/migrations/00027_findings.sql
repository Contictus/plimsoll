-- +goose Up

-- The data-quality register: everything we know to be wrong, or cannot account for.
--
-- A finding has a LIFETIME, not a timestamp (K53). Reconciliation runs every five minutes, so
-- a balance that disagrees all day is one problem the user has -- not 288 rows. A run opens
-- what it newly sees, touches what it still sees, and closes what it no longer sees.
--
-- This table is a projection, not the ledger: it is UPDATEd, and dropping it loses nothing,
-- because the next run rebuilds it from the ledger and the exchange (L2, L3).
CREATE TABLE findings (
  id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id     UUID        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  integration_id UUID        NOT NULL,

  -- A closed set, matching internal/quality's constants. The first four are the
  -- reconciliation classification (K54); the rest are the internal-coherence checks (K14).
  kind TEXT NOT NULL CHECK (kind IN (
         'missing_event', 'duplicate', 'rounding', 'unsupported',
         'negative_balance', 'unresolved_asset', 'fee_price_missing',
         'clock_skew', 'snapshot_failed')),

  -- What the finding is ABOUT, in the subject's own vocabulary: an asset code, an instrument
  -- id, or the empty string for a finding about the integration itself. Never a raw exchange
  -- symbol used as a key (L8) -- this column is a label the user reads, and joins go through
  -- the instrument id carried in detail.
  subject  TEXT NOT NULL DEFAULT '',

  severity TEXT NOT NULL CHECK (severity IN ('info', 'warn', 'error')),
  detail   TEXT NOT NULL DEFAULT '',

  -- The size of the disagreement, in the subject's own units. NUMERIC, like every other
  -- number in this system (L1). NULL where the finding has no magnitude -- a failed snapshot
  -- has no delta, and writing zero would claim we measured agreement.
  delta    NUMERIC(38,18),

  -- The exchange payload the finding was decided from (L15). When a classification turns out
  -- wrong in three months, this is the only thing that can settle the argument.
  raw      JSONB,

  opened_at    TIMESTAMPTZ NOT NULL,
  last_seen_at TIMESTAMPTZ NOT NULL,
  closed_at    TIMESTAMPTZ,
  occurrences  INT         NOT NULL DEFAULT 1,

  CONSTRAINT findings_close_after_open CHECK (closed_at IS NULL OR closed_at >= opened_at),
  CONSTRAINT findings_seen_after_open  CHECK (last_seen_at >= opened_at),
  CONSTRAINT findings_seen_at_least_once CHECK (occurrences >= 1),

  FOREIGN KEY (account_id, integration_id)
    REFERENCES integrations (account_id, id) ON DELETE CASCADE
);

-- Identity holds only WHILE OPEN. Closed rows fall outside the index, which is what makes a
-- problem that returns after closing a NEW finding rather than a resurrection: "wrong for an
-- hour, right for a day, wrong again" is two incidents, and collapsing them into one would
-- erase the recovery in between (K53).
CREATE UNIQUE INDEX findings_one_open_per_subject
  ON findings (account_id, integration_id, kind, subject)
  WHERE closed_at IS NULL;

-- The endpoint's query: what is wrong right now, worst first, newest first.
CREATE INDEX findings_open_by_account_idx
  ON findings (account_id, severity, opened_at DESC)
  WHERE closed_at IS NULL;

ALTER TABLE findings ENABLE ROW LEVEL SECURITY;
ALTER TABLE findings FORCE  ROW LEVEL SECURITY;

CREATE POLICY findings_own ON findings
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

-- No DELETE. A finding closes; it does not vanish. The history of a finding is the evidence
-- for its classification, and K55 makes that evidence the precondition for ever acting on one
-- automatically.
GRANT SELECT, INSERT, UPDATE ON findings TO plimsoll_app;

-- +goose Down
DROP TABLE findings;
