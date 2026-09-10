-- +goose Up

-- The ledger cannot answer "how close am I to liquidation".
--
-- Everything else in this system is a fold over events (L3), and that is what makes it
-- rebuildable and checkable. The exchange's margin engine is not: nothing in the ledger
-- says what Binance believed the maintenance requirement was at 12:00, and no amount of
-- replaying fills reconstructs it. It has to be captured.
--
-- So these tables are snapshots, and they are explicitly NOT projections. They get no
-- rebuild-equality test, because there is nothing to rebuild them from. What they get
-- instead is freshness: a stale snapshot is served with a reason attached, never silently
-- (L11), because during the market event that makes someone open this page, a slightly old
-- margin buffer is worth a great deal and a blank one is worth nothing.
--
-- Latest-only, upserted per integration. Historical equity lives in equity_snapshots when
-- drawdown arrives (M6, PROJECT.md section 4); keeping a row per capture here would be
-- inventing that table early and under the wrong name.
CREATE TABLE collateral_snapshots (
  account_id     UUID           NOT NULL,
  integration_id UUID           NOT NULL PRIMARY KEY,

  -- The EARLIER of the two calls this was made of. Maintenance margin comes from
  -- /fapi/v3/account and the liquidation price from /fapi/v3/positionRisk (F14), so a
  -- capture is two instants -- and a snapshot is only as fresh as its stalest half. Claiming
  -- otherwise reports a number as current on the strength of whichever call happened to be
  -- quick.
  as_of          TIMESTAMPTZ    NOT NULL,

  -- Ours, never used in a calculation. It exists so a reader can tell "the exchange's
  -- number is old" from "we have not asked recently", which are different problems with
  -- different fixes.
  captured_at    TIMESTAMPTZ    NOT NULL DEFAULT now(),

  margin_balance     NUMERIC(38,18) NOT NULL,
  wallet_balance     NUMERIC(38,18) NOT NULL,
  unrealized_pnl     NUMERIC(38,18) NOT NULL,
  maintenance_margin NUMERIC(38,18) NOT NULL,
  available_balance  NUMERIC(38,18) NOT NULL,

  FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id)
);

-- One row per position the venue reported, replaced wholesale with its snapshot.
--
-- instrument_id rather than the exchange symbol, resolved at capture time: a raw symbol is
-- never a key (L8, K10), and "BTCUSDT" here would be the spot pair as readily as the perp.
CREATE TABLE collateral_positions (
  account_id     UUID           NOT NULL,
  integration_id UUID           NOT NULL,
  instrument_id  BIGINT         NOT NULL REFERENCES instruments (id),

  quantity       NUMERIC(38,18) NOT NULL,
  entry_price    NUMERIC(38,18) NOT NULL,
  mark_price     NUMERIC(38,18) NOT NULL,

  -- Read from the venue, never computed (K6). It depends on margin-tier tables, cross
  -- versus isolated margin and wallet interactions that drift without notice; we inherit
  -- the exchange's staleness because a confidently wrong liquidation price is far more
  -- dangerous than a slightly late correct one. Zero means the venue reported none, which
  -- is what a flat position gets -- and is why the API renders no distance rather than an
  -- infinite one.
  liquidation_price NUMERIC(38,18) NOT NULL,

  notional       NUMERIC(38,18) NOT NULL,
  leverage       NUMERIC(38,18) NOT NULL,
  maint_margin   NUMERIC(38,18) NOT NULL,

  PRIMARY KEY (integration_id, instrument_id),
  FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id),
  FOREIGN KEY (integration_id) REFERENCES collateral_snapshots (integration_id) ON DELETE CASCADE
);

-- The venue's maintenance-margin tier table (F15), per integration because a user's
-- brackets depend on their own tier.
--
-- Captured in M5 rather than in M7.5 because M7.5 is a pure function (L4) and a pure
-- function cannot go and fetch what it was not given. Without this table a scenario shock
-- has to scale today's maintenance margin by the price move -- which silently assumes the
-- rate is constant, and is wrong precisely when the shock is large enough to matter, in the
-- direction that understates the danger.
CREATE TABLE leverage_brackets (
  account_id     UUID           NOT NULL,
  integration_id UUID           NOT NULL,
  instrument_id  BIGINT         NOT NULL REFERENCES instruments (id),

  -- The venue's own tier number, and the ordering within a symbol.
  bracket        INT            NOT NULL,

  notional_floor NUMERIC(38,18) NOT NULL,
  notional_cap   NUMERIC(38,18) NOT NULL,

  -- A rate, and therefore money (L1). Stored at the same scale as everything else so a
  -- product of it never picks up a different rounding.
  maint_margin_ratio NUMERIC(38,18) NOT NULL,

  -- What makes the tiers continuous instead of a step function jumping at every boundary.
  cum            NUMERIC(38,18) NOT NULL,

  captured_at    TIMESTAMPTZ    NOT NULL DEFAULT now(),

  PRIMARY KEY (integration_id, instrument_id, bracket),
  FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id),

  CONSTRAINT brackets_span_upward CHECK (notional_cap > notional_floor)
);

ALTER TABLE collateral_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE collateral_snapshots FORCE  ROW LEVEL SECURITY;
ALTER TABLE collateral_positions ENABLE ROW LEVEL SECURITY;
ALTER TABLE collateral_positions FORCE  ROW LEVEL SECURITY;
ALTER TABLE leverage_brackets    ENABLE ROW LEVEL SECURITY;
ALTER TABLE leverage_brackets    FORCE  ROW LEVEL SECURITY;

CREATE POLICY collateral_snapshots_own ON collateral_snapshots
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

CREATE POLICY collateral_positions_own ON collateral_positions
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

CREATE POLICY leverage_brackets_own ON leverage_brackets
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

GRANT SELECT, INSERT, UPDATE, DELETE
  ON collateral_snapshots, collateral_positions, leverage_brackets TO plimsoll_app;

-- +goose Down
DROP TABLE leverage_brackets;
DROP TABLE collateral_positions;
DROP TABLE collateral_snapshots;
