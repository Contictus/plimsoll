-- +goose Up

-- What the account holds, as opposed to what its positions cost.
--
-- M3 shipped a portfolio made of positions, which is the right answer for a derivatives
-- account and only half of one for a spot account: "I am long 0.5 BTC at 60000" does not
-- say how much USDT is left. Both come from the same events, and this is the other fold
-- over them.
--
-- A projection on the same terms as positions (L3): droppable, rebuildable to the same
-- rows, and sharing the projection cursor rather than keeping one of its own -- two
-- cursors over one event stream are two chances to disagree about what has been folded.
CREATE TABLE asset_balances (
  account_id     UUID           NOT NULL,
  integration_id UUID           NOT NULL,
  asset_id       BIGINT         NOT NULL REFERENCES assets (id),

  -- Signed, and negative is legal here on purpose. A negative balance is not a storage
  -- error, it is the strongest data-quality signal there is: the ledger implies selling
  -- more than was ever held, so an event is missing (K14). A CHECK forbidding it would
  -- turn the finding into a crash and lose the evidence.
  quantity       NUMERIC(38,18) NOT NULL,

  last_event_time     TIMESTAMPTZ NOT NULL,
  last_venue_sequence BIGINT      NOT NULL,
  last_venue_event_id TEXT        NOT NULL,
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

  PRIMARY KEY (integration_id, asset_id),
  FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id)
);

ALTER TABLE asset_balances ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_balances FORCE  ROW LEVEL SECURITY;

CREATE POLICY asset_balances_own ON asset_balances
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

GRANT SELECT, INSERT, UPDATE, DELETE ON asset_balances TO plimsoll_app;

-- The fee's asset, resolved once at ingest as of the event's own event_time (L8, K22).
--
-- fee_asset already holds the exchange's ticker, and that is not a key: resolving it at
-- fold time would mean resolving it with whatever mapping is current then, which is the
-- industry's number-one silent corruption. Resolving it at ingest is the only moment the
-- event's own time is unambiguously in hand.
--
-- Nullable, and it must stay nullable. A fee asset that does not resolve is a real
-- possibility -- a coin we have not curated yet -- and rejecting the trade over it would
-- lose a fill to keep a fee. The trade stores, the fee's balance effect does not apply,
-- and the reader is told through unknown_symbol rather than left with a quiet shortfall
-- (L11). Rows written before this column existed are NULL for the same reason and read
-- the same way.
ALTER TABLE ledger_events ADD COLUMN fee_asset_id BIGINT REFERENCES assets (id);

-- +goose Down
ALTER TABLE ledger_events DROP COLUMN fee_asset_id;
DROP TABLE asset_balances;
