-- +goose Up

-- The two halves of a cross-venue movement, joined.
--
-- K49 settled the intra-venue case: one row names both wallets and there is nothing to match.
-- This is the case K12 actually described -- a withdrawal under one integration and a deposit
-- under another, two events no venue will ever say are the same movement.
--
-- A link changes NO number. The deposit already added and the withdrawal already subtracted,
-- on two different integrations, and both were right. What the link changes is the reading: a
-- withdrawal taken for a disposal invents a realized loss, and this is the row that says it
-- was not one (K57).
CREATE TABLE transfer_links (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,

  -- The ledger rows themselves. seq is the row's identity here, which is a different use from
  -- the cursor L6 forbids: nothing advances on it, it is only a name for one row.
  out_seq BIGINT NOT NULL REFERENCES ledger_events (seq),
  in_seq  BIGINT NOT NULL REFERENCES ledger_events (seq),

  -- How it was decided. A link from a chain transaction and one from a guess are different
  -- claims, and a user resolving a dispute has to be able to tell them apart (K57).
  method TEXT NOT NULL CHECK (method IN ('txid', 'heuristic', 'manual')),

  linked_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- A leg belongs to at most one transfer, in each direction. This is the constraint that
  -- stops one deposit settling two withdrawals -- the failure an account moving the same round
  -- number twice a week produces, where the second link says the money went somewhere it did
  -- not. Enforced here as well as in the matcher, because the manual endpoint writes rows the
  -- matcher never saw.
  CONSTRAINT transfer_links_one_out UNIQUE (out_seq),
  CONSTRAINT transfer_links_one_in  UNIQUE (in_seq),

  -- A leg cannot be both halves of its own transfer.
  CONSTRAINT transfer_links_two_legs CHECK (out_seq <> in_seq)
);

CREATE INDEX transfer_links_by_account_idx ON transfer_links (account_id, linked_at DESC);

ALTER TABLE transfer_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE transfer_links FORCE  ROW LEVEL SECURITY;

CREATE POLICY transfer_links_own ON transfer_links
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

-- DELETE is granted here, unlike findings: a link is an assertion about two events, not a
-- record of what happened. A user who joined the wrong two legs must be able to unjoin them,
-- and the events themselves are untouched either way (L2).
GRANT SELECT, INSERT, UPDATE, DELETE ON transfer_links TO plimsoll_app;

-- The register gains the matcher's own kind. A leg with no other half is a data-quality
-- finding like any other, and it closes the same way: when the leg is finally linked, the
-- next pass does not see it and the finding closes itself (K53).
ALTER TABLE findings DROP CONSTRAINT findings_kind_check;
ALTER TABLE findings ADD CONSTRAINT findings_kind_check CHECK (kind IN (
  'missing_event', 'duplicate', 'rounding', 'unsupported',
  'negative_balance', 'unresolved_asset', 'fee_price_missing',
  'clock_skew', 'snapshot_failed', 'unmatched_transfer'));

-- +goose Down
ALTER TABLE findings DROP CONSTRAINT findings_kind_check;
ALTER TABLE findings ADD CONSTRAINT findings_kind_check CHECK (kind IN (
  'missing_event', 'duplicate', 'rounding', 'unsupported',
  'negative_balance', 'unresolved_asset', 'fee_price_missing',
  'clock_skew', 'snapshot_failed'));
DROP TABLE transfer_links;
