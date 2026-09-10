-- +goose Up

-- What the user asked to be told about.
--
-- The two thresholds are the point. `trigger` is where the condition starts and `clear` is
-- where it ends, and they are DIFFERENT numbers: one threshold approached from both sides is
-- what makes a metric sitting on it fire at every evaluation, twenty messages in ten minutes,
-- and a user who silences the channel. After that the alerting has negative value -- worse
-- than never having built it, because the user believes something is watching.
CREATE TABLE alert_rules (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id UUID        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  name       TEXT        NOT NULL,

  -- A closed set, matching internal/alert's constants: a rule naming a metric nothing
  -- computes is a rule that never fires, and silence is the one failure this feature cannot
  -- report about itself.
  metric     TEXT        NOT NULL CHECK (metric IN (
               'leverage', 'net_leverage', 'gross_exposure',
               'margin_buffer', 'liquidation_distance', 'concentration')),

  scope_kind TEXT        NOT NULL DEFAULT 'portfolio'
             CHECK (scope_kind IN ('portfolio', 'strategy', 'position')),
  -- Empty for the portfolio scope; the strategy's name or the position's id otherwise.
  scope_name TEXT        NOT NULL DEFAULT '',

  comparator TEXT        NOT NULL CHECK (comparator IN ('above', 'below')),

  -- Money, and therefore NUMERIC(38,18) like everything else (L1). A leverage ratio decided
  -- by a float is decided in the digits nobody checked.
  trigger_at NUMERIC(38,18) NOT NULL,
  clear_at   NUMERIC(38,18) NOT NULL,

  cooldown_seconds INT     NOT NULL DEFAULT 0 CHECK (cooldown_seconds >= 0),
  enabled          BOOLEAN NOT NULL DEFAULT true,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- The band has to point the right way, or the rule fires and clears on the same tick and
  -- the hysteresis is decoration. An 'above' rule clears BELOW its trigger; a 'below' rule
  -- clears above it.
  CONSTRAINT alert_band_points_the_right_way CHECK (
    (comparator = 'above' AND clear_at <= trigger_at) OR
    (comparator = 'below' AND clear_at >= trigger_at)),

  CONSTRAINT alert_rules_named_once_per_account UNIQUE (account_id, name)
);

-- What the evaluator remembers between runs. Separate from the rule because it is not
-- configuration: dropping it makes every rule evaluate as if it had never fired, which is
-- recoverable noise, while dropping a rule loses thresholds a human tuned.
CREATE TABLE alert_state (
  rule_id       UUID        PRIMARY KEY REFERENCES alert_rules (id) ON DELETE CASCADE,
  account_id    UUID        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  firing        BOOLEAN     NOT NULL DEFAULT false,
  since         TIMESTAMPTZ,
  last_fired_at TIMESTAMPTZ
);

-- Every firing and every recovery, kept. The alert row exists whether or not the message left
-- the building: delivery is transport, and this is the record -- so an account whose Telegram
-- token expired still has a history of what happened while nobody was being told.
CREATE TABLE alerts (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id UUID        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  rule_id    UUID        NOT NULL REFERENCES alert_rules (id) ON DELETE CASCADE,

  kind       TEXT        NOT NULL CHECK (kind IN ('fired', 'resolved', 'unavailable')),
  metric     TEXT        NOT NULL,
  scope_kind TEXT        NOT NULL,
  scope_name TEXT        NOT NULL DEFAULT '',

  -- Null for 'unavailable', and never zero: a metric nobody could compute and a metric that
  -- is zero are opposite claims (L11).
  value      NUMERIC(38,18),

  -- The valuation run this was decided from. Alerts evaluate on completed runs and not per
  -- tick (ARCHITECTURE section 7), and carrying the run means an alert can be traced to the
  -- prices behind it -- which is the same claim the rest of this system makes about numbers.
  run_id     BIGINT,

  fired_at     TIMESTAMPTZ NOT NULL,
  delivered_at TIMESTAMPTZ,
  -- Redacted by construction: whatever went wrong with a channel, the channel's own
  -- credential is not written here (L13).
  delivery_error TEXT
);

CREATE INDEX alerts_by_account_time_idx ON alerts (account_id, fired_at DESC);

ALTER TABLE alert_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_rules FORCE  ROW LEVEL SECURITY;
ALTER TABLE alert_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_state FORCE  ROW LEVEL SECURITY;
ALTER TABLE alerts      ENABLE ROW LEVEL SECURITY;
ALTER TABLE alerts      FORCE  ROW LEVEL SECURITY;

CREATE POLICY alert_rules_own ON alert_rules
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());
CREATE POLICY alert_state_own ON alert_state
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());
CREATE POLICY alerts_own ON alerts
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

GRANT SELECT, INSERT, UPDATE, DELETE ON alert_rules, alert_state, alerts TO plimsoll_app;

-- +goose Down
DROP TABLE alerts;
DROP TABLE alert_state;
DROP TABLE alert_rules;
