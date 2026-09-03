-- +goose Up

-- Everything here is a projection (L3): droppable, and rebuildable from ledger_events to
-- the same rows. Nothing downstream may treat these tables as a source of truth, which is
-- why the app role holds DELETE on them -- the opposite of ledger_events, where it does
-- not and must not.

CREATE TABLE positions (
  account_id      UUID           NOT NULL,
  integration_id  UUID           NOT NULL,
  instrument_id   BIGINT         NOT NULL REFERENCES instruments (id),

  quantity        NUMERIC(38,18) NOT NULL,   -- signed: positive long, negative short
  avg_entry_price NUMERIC(38,18) NOT NULL,   -- unsigned; zero when flat
  realized_pnl    NUMERIC(38,18) NOT NULL,

  -- The last event folded into this row, as the whole canonical order (L7). PROJECT.md
  -- carries only last_venue_sequence; that is not enough, because two events can share a
  -- sequence and a sequence-only cursor rejects the second one and loses it for good.
  last_event_time      TIMESTAMPTZ NOT NULL,
  last_venue_sequence  BIGINT      NOT NULL,
  last_venue_event_id  TEXT        NOT NULL,

  updated_at      TIMESTAMPTZ    NOT NULL DEFAULT now(),

  PRIMARY KEY (integration_id, instrument_id),
  FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id)
);

-- Fees stay per asset and unconverted (K18, L9). A child table rather than a JSONB blob,
-- so a fee is a NUMERIC(38,18) the database can check rather than a string something has
-- to parse -- and so valuation can join it to a price in M3 instead of unnesting it.
CREATE TABLE position_fees (
  account_id     UUID           NOT NULL,
  integration_id UUID           NOT NULL,
  instrument_id  BIGINT         NOT NULL,
  fee_asset      TEXT           NOT NULL,
  amount         NUMERIC(38,18) NOT NULL,

  PRIMARY KEY (integration_id, instrument_id, fee_asset),
  FOREIGN KEY (integration_id, instrument_id)
    REFERENCES positions (integration_id, instrument_id) ON DELETE CASCADE
);

-- Where the projector got to for this integration. Per integration and protected by the
-- single-writer lease, never the global seq: identity values are assigned before commit,
-- so a seq cursor can skip an event that was still in flight when it was read (K20, L6).
CREATE TABLE projection_cursors (
  account_id          UUID        NOT NULL,
  integration_id      UUID        NOT NULL PRIMARY KEY,
  last_event_time     TIMESTAMPTZ NOT NULL,
  last_venue_sequence BIGINT      NOT NULL,
  last_venue_event_id TEXT        NOT NULL,
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

  FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id)
);

-- The strategy tag is user input, not a fold output, so it cannot live on positions (D2).
-- PROJECT.md lists it as a column there; a rebuild would erase every assignment, and the
-- rebuild-equality test would still pass because both sides would be equally empty.
--
-- strategy_id carries no foreign key yet: the strategies registry arrives with the sleeve
-- model in K13's milestone. This table exists now because the boundary it draws is a
-- schema decision, and schema decisions are the expensive ones to defer.
CREATE TABLE position_strategies (
  account_id     UUID        NOT NULL,
  integration_id UUID        NOT NULL,
  instrument_id  BIGINT      NOT NULL REFERENCES instruments (id),
  strategy_id    UUID        NOT NULL,
  assigned_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

  PRIMARY KEY (integration_id, instrument_id),
  FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id)
);

ALTER TABLE positions           ENABLE ROW LEVEL SECURITY;
ALTER TABLE positions           FORCE  ROW LEVEL SECURITY;
ALTER TABLE position_fees       ENABLE ROW LEVEL SECURITY;
ALTER TABLE position_fees       FORCE  ROW LEVEL SECURITY;
ALTER TABLE projection_cursors  ENABLE ROW LEVEL SECURITY;
ALTER TABLE projection_cursors  FORCE  ROW LEVEL SECURITY;
ALTER TABLE position_strategies ENABLE ROW LEVEL SECURITY;
ALTER TABLE position_strategies FORCE  ROW LEVEL SECURITY;

CREATE POLICY positions_own ON positions
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());
CREATE POLICY position_fees_own ON position_fees
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());
CREATE POLICY projection_cursors_own ON projection_cursors
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());
CREATE POLICY position_strategies_own ON position_strategies
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

GRANT SELECT, INSERT, UPDATE, DELETE
  ON positions, position_fees, projection_cursors, position_strategies TO plimsoll_app;

-- +goose Down
DROP TABLE position_strategies;
DROP TABLE projection_cursors;
DROP TABLE position_fees;
DROP TABLE positions;
