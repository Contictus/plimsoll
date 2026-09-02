-- +goose Up

-- integrations lands here, one milestone earlier than the connection flow that fills it,
-- because ledger_events carries a composite FK to it. Adding that FK to a populated
-- ledger means rebuilding the table -- exactly the rebuild M0 existed to make unnecessary.
-- The credential columns (credential_ciphertext, wrapped_dek, key_version) arrive in M2
-- with envelope encryption (K25); this table holds only identity and tenancy until then.
CREATE TABLE integrations (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id UUID        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  exchange   TEXT        NOT NULL,
  label      TEXT        NOT NULL,
  status     TEXT        NOT NULL DEFAULT 'active'
               CHECK (status IN ('active', 'paused', 'revoked')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- Redundant against the primary key, and required: it is the target of ledger_events'
  -- composite foreign key, which is what makes a cross-account event unstorable.
  UNIQUE (account_id, id)
);

ALTER TABLE integrations ENABLE ROW LEVEL SECURITY;
ALTER TABLE integrations FORCE  ROW LEVEL SECURITY;

CREATE POLICY integrations_own ON integrations
  USING (account_id = app_current_account())
  WITH CHECK (account_id = app_current_account());

-- Read-only for now. The write path arrives in M2 together with credential encryption and
-- the read-only key verification (K9), so until then a new table stays unreachable rather
-- than reachable-by-default.
GRANT SELECT ON integrations TO plimsoll_app;

CREATE TABLE ledger_events (
  -- Lineage and debugging only. NEVER a projection cursor: identity values are assigned
  -- before commit, so a reader can see seq=100 committed while seq=99 is still in flight,
  -- and a last_seq cursor would skip it permanently (K20, L6). GENERATED ALWAYS rather
  -- than BIGSERIAL so that no writer can supply one and pretend to control the order.
  seq            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

  account_id     UUID        NOT NULL,
  integration_id UUID        NOT NULL,

  -- <market>:<kind>:<symbol>:<venue id>, built by the normalizer from exchange fields
  -- only -- never from our clock, our counters, or the ingest path. Two ingest paths
  -- seeing one exchange event must produce the same string; that property is what makes
  -- ON CONFLICT DO NOTHING a correctness mechanism instead of a performance trick.
  venue_event_id TEXT        NOT NULL,

  -- The exchange's own monotonic id, and the second level of the canonical order (K21).
  -- NOT NULL because the order is compared as a whole tuple: a NULL here would make the
  -- keyset comparison undefined and let the fold skip events. Where a venue supplies no
  -- id of its own the normalizer writes 0, which sorts first within its timestamp.
  venue_sequence BIGINT      NOT NULL,

  -- Metadata: who saw it first. Deliberately NOT part of the identity (K19, L5) -- REST
  -- backfill and the WS stream report the same trade under different values here, and a
  -- key including source would insert both and double the position.
  source         TEXT        NOT NULL,

  event_type     TEXT        NOT NULL CHECK (event_type IN (
                   'TRADE', 'TRANSFER', 'DEPOSIT', 'WITHDRAWAL', 'FUNDING_PAYMENT',
                   'FEE', 'COMMISSION_REBATE', 'LIQUIDATION', 'POSITION_ADJUSTMENT')),

  instrument_id  BIGINT      REFERENCES instruments (id),
  strategy_id    UUID,
  side           TEXT        CHECK (side IN ('buy', 'sell')),

  quantity       NUMERIC(38,18),
  price          NUMERIC(38,18),

  -- The fee belongs to the event that caused it (K18, L9). The standalone FEE event type
  -- is only for fees with no parent; never both, or it is counted twice.
  fee            NUMERIC(38,18),
  fee_asset      TEXT,

  -- The exchange's own time. Everything downstream is calculated from it (K2); ingested_at
  -- is ours and is never used in a computation.
  event_time     TIMESTAMPTZ NOT NULL,
  ingested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- Mandatory. When a normalization bug surfaces months later, replaying the ledger from
  -- raw is the thing that saves the project (L15).
  raw            JSONB       NOT NULL,

  CONSTRAINT ledger_events_venue_identity UNIQUE (integration_id, venue_event_id),

  CONSTRAINT ledger_integration_belongs_to_account
    FOREIGN KEY (account_id, integration_id) REFERENCES integrations (account_id, id),

  -- A fill without an instrument, a side, a quantity or a price is not a fill; storing one
  -- would put a hole in the fold that only shows up as a wrong average entry price.
  CONSTRAINT fills_are_fully_specified CHECK (
    event_type NOT IN ('TRADE', 'LIQUIDATION')
    OR (instrument_id IS NOT NULL AND side IS NOT NULL
        AND quantity IS NOT NULL AND quantity > 0
        AND price IS NOT NULL AND price >= 0)),

  -- Direction lives on side, magnitude on quantity. A funding payment or an adjustment may
  -- legitimately be negative, so the sign is only constrained where side carries it.
  CONSTRAINT fee_names_its_asset CHECK ((fee IS NULL) = (fee_asset IS NULL))
);

-- The canonical order in full (L7), so the index serves the fold's ORDER BY and its keyset
-- cursor without a sort. PROJECT.md lists three columns; venue_event_id is the fourth
-- level of that same order and belongs here for the comparison to be covered end to end.
CREATE INDEX ledger_events_canonical_order_idx
  ON ledger_events (integration_id, event_time, venue_sequence, venue_event_id);

ALTER TABLE ledger_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_events FORCE  ROW LEVEL SECURITY;

-- Two policies, and deliberately no UPDATE and no DELETE policy: there is no legitimate
-- update to a ledger row from any role, and a correction is a new event (L2). FORCE means
-- this binds the table owner too, so even a migration cannot quietly rewrite history.
CREATE POLICY ledger_events_read ON ledger_events
  FOR SELECT USING (account_id = app_current_account());
CREATE POLICY ledger_events_append ON ledger_events
  FOR INSERT WITH CHECK (account_id = app_current_account());

-- Append-only as a privilege rather than a convention. The identity column needs no
-- separate sequence grant, which is a second reason to prefer it over BIGSERIAL.
GRANT SELECT, INSERT ON ledger_events TO plimsoll_app;

-- +goose Down
DROP TABLE ledger_events;
DROP TABLE integrations;
