# Plimsoll — Project Specification

Backend-focused position/risk engine with an ingestion and reconciliation layer.
The UI is secondary; the product is correct portfolio state derived from a ledger.

**Positioning:** for the leveraged CEX trader — a canonical, reconciled,
strategy-aware realtime risk engine built on an append-only ledger. Retail trackers show
balances, tax tools keep history correct, institutional PMS platforms do both but are
out of reach. Full reasoning: `COMPETITIVE-ANALYSIS.md`.

**The axis we will not compete on: integration count.** Differentiation is not coverage,
it is accuracy and risk depth. The most visible feature of this product is not how many
exchanges it supports — it is reconciliation status and the traceability of every number
down to its events.

**Companion documents**

| Document | Contains |
|---|---|
| `DECISIONS.md` | K1–K28: every architectural decision, its rationale and its cost |
| `ARCHITECTURE.md` | Module boundaries, data flow, tenancy mechanics, worker model, schema deltas |
| `COMPETITIVE-ANALYSIS.md` | Market segmentation, the gap, competitor failure modes |
| `../CLAUDE.md` = `../AGENTS.md` | Agent operating manual: invariants, workflow, definition of done |

This file states **what** is being built. `DECISIONS.md` states **why**.
`ARCHITECTURE.md` states **how the pieces fit**. Content is not duplicated between them.

---

## 1. V1 Scope (locked)

**In**

- Binance (spot + USD-M perpetual), read-only API key
- Historical backfill + realtime user data stream
- Canonical ledger, position engine, PnL, portfolio, exposure/leverage
- Market data ingest, price history, mark-to-market
- Reconciliation (our state ↔ exchange snapshot)
- Risk thresholds and alerting
- Realtime dashboard over SSE
- **Multi-user**: invite-provisioned accounts, password sessions, tenant isolation (K15, K16)
- **USD numeraire** with priced stablecoins (K17)

**Out (V1)**

- Bybit / Coinbase — V2, and the real test of the normalization layer
- EVM wallets, Solana, DeFi, LP positions
- COIN-M futures, options
- Hedge mode (two-sided positions)
- FIFO/LIFO tax accounting — but the ledger stays lot-derivable from day one (K5)
- Multi-asset / portfolio margin
- **Trade execution — never.** The API key permission will not allow it.

---

## 2. Canonical Model

### Asset and Instrument

An exchange symbol is not a canonical instrument, and an instrument is not an asset (K10).
Two layers:

```
assets
  id
  canonical_symbol      BTC / ETH / USDT
  kind                  native | token | stablecoin | fiat
  chain                 ethereum          (tokens)
  contract_address      0x...             (tokens)
  is_wrapped            WBTC → underlying_asset_id = BTC

asset_aliases
  source                binance | bybit | coingecko
  external_symbol       BTC
  asset_id              FK
  valid_from, valid_to  ◀── K22, time-scoped resolution

instruments
  id
  canonical_symbol      BTC-USDT-PERP / BTC-USDT-SPOT
  kind                  spot | perp
  base_asset, quote_asset, settle_asset
  contract_size         NUMERIC

instrument_aliases
  exchange              binance
  exchange_symbol       BTCUSDT
  market                spot | usdm      ◀── part of the key, not a detail
  instrument_id         FK
  valid_from, valid_to  ◀── K22
```

On Binance, spot `BTCUSDT` and perpetual `BTCUSDT` are the same string and different
instruments. Without this split, positions merge and everything downstream is wrong.
Get it right on day one — retrofitting means rebuilding the ledger.

### Ledger event types

```
TRADE                buy / sell
TRANSFER             between accounts (spot ↔ futures)
DEPOSIT
WITHDRAWAL
FUNDING_PAYMENT      perp funding — affects realized PnL
FEE                  only when it has no parent event (K18)
COMMISSION_REBATE
LIQUIDATION          a special case of trade, flagged separately
POSITION_ADJUSTMENT  ADL, settlement, etc.
```

### Schema (summary)

```sql
accounts   (id, email, password_hash, created_at, disabled_at);
sessions   (token_hash, account_id, created_at, expires_at, last_seen_at);
invites    (token_hash, created_by, consumed_by, expires_at);

integrations (
  id, account_id, exchange, label,
  credential_ciphertext BYTEA,    -- envelope-encrypted (K25)
  wrapped_dek           BYTEA,
  key_version           INT,
  status, created_at,
  UNIQUE (account_id, id)         -- target of the composite FK below
);

ledger_events (
  seq             BIGSERIAL PRIMARY KEY,   -- lineage/debug only, NEVER a cursor (K20)
  account_id      UUID NOT NULL,           -- denormalized for RLS (K15)
  integration_id  UUID NOT NULL,
  venue_event_id  TEXT NOT NULL,           -- source-independent identity (K19)
  venue_sequence  BIGINT,                  -- exchange's own id, ordering tiebreak (K21)
  source          TEXT NOT NULL,           -- metadata only: who saw it first
  event_type      TEXT NOT NULL,
  instrument_id   BIGINT,
  strategy_id     UUID,                    -- K13
  side            TEXT,                    -- buy | sell
  quantity        NUMERIC(38,18),
  price           NUMERIC(38,18),
  fee             NUMERIC(38,18),          -- belongs to THIS event only (K18)
  fee_asset       TEXT,
  event_time      TIMESTAMPTZ NOT NULL,    -- exchange time; drives all calculation (K2)
  ingested_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  raw             JSONB NOT NULL,          -- mandatory; the project's insurance policy
  UNIQUE (integration_id, venue_event_id), -- K19
  FOREIGN KEY (account_id, integration_id)
    REFERENCES integrations (account_id, id)
);
CREATE INDEX ON ledger_events (integration_id, event_time, venue_sequence);

positions (                       -- projection: droppable and rebuildable (L3)
  account_id, integration_id, instrument_id, strategy_id,
  quantity, avg_entry_price, realized_pnl,
  last_venue_sequence,            -- per-integration cursor (K20)
  updated_at,
  PRIMARY KEY (integration_id, instrument_id)
);

position_snapshots  (account_id, integration_id, as_of, venue_sequence, state JSONB);
price_ticks         (instrument_id, ts, mark_price, index_price);   -- BRIN on ts
equity_snapshots    (account_id, ts, equity_usd);                   -- drawdown input

integration_leases  (integration_id PK, owner_id, heartbeat_at, expires_at);  -- K20
backfill_progress   (integration_id, scope, cursor, completed_at);            -- K26

valuation_runs      (id, account_id, as_of, numeraire, price_source,
                     assumed_peg, freshness JSONB);                 -- K11, K17, K23
valuation_prices    (run_id, asset_id, price_usd, path JSONB, source, observed_at);

exchange_snapshots  (account_id, integration_id, taken_at, kind, payload JSONB);
reconciliation_runs     (id, account_id, integration_id, started_at, status);
reconciliation_findings (run_id, instrument_id, ours, theirs, delta, severity, class);

alert_rules (account_id, integration_id, metric, operator, threshold,
             enabled, cooldown_seconds, hysteresis_pct);
alerts      (rule_id, fired_at, cleared_at, snapshot JSONB);

strategies  (id, account_id, name, kind);   -- basis_trade | directional | hedge

transfer_links (                            -- K12
  id, account_id,
  out_event_seq, in_event_seq,
  match_method,          -- txid | heuristic | manual
  confidence, confirmed_by_user
);

data_quality_findings (account_id, integration_id, detected_at, check_name,
                       severity, instrument_id, details JSONB, resolved_at);  -- K14
fx_rates              (base_asset, quote_ccy, ts, rate);
```

Every tenant table carries `account_id`, has RLS enabled and forced, and is only reached
through `tenancy.InTx` (K15).

**Storing the raw payload is mandatory.** When a normalization bug surfaces, rebuilding
the ledger from `raw` is what saves the project.

---

## 3. Critical Flows

Detailed in `ARCHITECTURE.md` §6–§8. Summary:

| Flow | Shape | Key property |
|---|---|---|
| **Backfill** | verify key → discover instruments → spot trades, futures trades + income, deposits/withdrawals → idempotent insert → rebuild | Chunked and resumable; `backfill_incomplete` in `freshness` until done (K26) |
| **Realtime** | listenKey → user stream → normalize → insert → project → SSE | Single writer per integration, held by lease (K20) |
| **Gap handling** | disconnect / missed keepalive / sequence hole → position not live → bounded REST resync → `ws_gap` in `freshness` | Never guess across a gap, never hide one |
| **Market data → risk** | mark price WS → Redis latest + minutely `price_ticks` → valuation → thresholds → alert | Alerts evaluate on completed valuation runs, not per tick |
| **Reconciliation** | every 5 min: REST snapshot → compare → classified findings | V1: detect and report. **No auto-correction** until classification is validated |

---

## 4. Risk Metrics (V1)

```
equity              = cash + spot value + perp unrealized PnL      (in USD, K17)
gross_exposure      = Σ |notional|
net_exposure        = Σ  notional
asset_exposure      = net notional per asset
leverage            = gross_exposure / equity
concentration       = |asset_exposure| / gross_exposure
unrealized_pnl      = Σ (mark − avg_entry) × qty × direction
realized_pnl        = cumulative from ledger, including funding and fees (K18)
drawdown            = decline from peak equity (equity_snapshots)
liq_distance        = |mark − liq_price| / mark    (liq_price from exchange, K6)
margin_utilization  = used margin / available margin
funding_cost        = periodic funding total
```

**Per strategy (K13):** the same metrics computed by `strategy_id`. In delta-neutral
setups, portfolio-level gross exposure alone is actively misleading;
`net_delta_per_asset` is reported inside the strategy. See `ARCHITECTURE.md` §9.

**Collateral (M5 — its own domain, not a field on the risk engine):**

```
margin_balance           per venue
maintenance_margin_rate  MMR — intra-venue leverage
margin_buffer            equity − maintenance margin
loan_to_value            LTV — extra-venue collateralized borrowing (V2)
```

**Scenario shock (M7.5):** cheap, because the engine is a pure function (L4).
`POST /risk/scenario` takes `{"BTC": -0.20, "ETH": -0.25}`, shocks the prices, re-runs
valuation and returns equity, margin buffer and distance to liquidation. For a leveraged
user this is the single most valuable feature in the product.

---

## 5. API

```
POST   /auth/login                          POST /auth/logout
POST   /auth/accept-invite

GET    /portfolio                     ?at=<RFC3339>    historical reconstruction
GET    /portfolio/history             ?from&to&interval
GET    /positions                     GET /positions/{id}
GET    /positions/{id}/lineage        events + prices that produced this number
PUT    /positions/{id}/strategy
GET    /pnl                           ?from&to
GET    /exposure                      GET /risk
POST   /risk/scenario                 price shock (M7.5)
GET    /transactions                  ledger, cursor-paginated on seq
GET    /funding

GET    /integrations                  POST /integrations/binance
DELETE /integrations/{id}
GET    /reconciliation
GET    /data-quality                  open findings by severity (K14)
GET    /assets                        canonical registry + alias resolution
GET    /transfers                     ?unmatched=true
POST   /transfers/{out}/link/{in}     manual transfer matching
GET    /strategies                    POST /strategies
GET    /alerts
GET    /alert-rules                   PUT /alert-rules/{id}

GET    /stream/portfolio   (SSE)      GET /stream/risk   (SSE)
GET    /stream/positions   (SSE)
```

**Contract rules (all endpoints, no exceptions)** — see `ARCHITECTURE.md` §10:
all numbers are JSON strings; every response carries `as_of` and `freshness`; every total
in a response comes from one `valuation_run`; no endpoint writes ledger events; no
endpoint ever places an order.

---

## 6. Stack

| Layer | Choice | Note |
|---|---|---|
| Language | Go | |
| API | Huma v2 + chi | Handler-derived OpenAPI; no hand-written spec to drift |
| DB | PostgreSQL | `NUMERIC(38,18)`, RLS, JSONB, BRIN, advisory locks, `EXCLUDE USING gist` |
| DB access | pgx v5 + sqlc | decimal override; generated output is committed |
| Migration | goose | runs as `plimsoll_owner`, never the app role |
| Money | shopspring/decimal | K4 |
| Cache / bus | Redis | latest-price hash + SSE fan-out only (K28) |
| Realtime out | SSE | |
| Realtime in | Exchange WebSocket | `coder/websocket` — context-aware cancellation |
| Password | argon2id (`x/crypto/argon2`) | K16 |
| Session | opaque token, hashed in Postgres | not JWT — must be revocable (K16) |
| Encryption | AES-256-GCM, envelope | per-account DEK behind a `KeyProvider` interface (K25) |
| Rate limit | `x/time/rate`, two tiers | `WaitN(ctx, weight)` models endpoint cost (K24) |
| Logging | `log/slog` | structured, correlated to OTel traces |
| Observability | OpenTelemetry + Prometheus + Grafana | |
| Testing | stdlib + testify/require + go-cmp | integration tests need a real Postgres |
| TLS / routing | Caddy, same origin | no CORS, token unreachable from JS (K27) |
| Local & prod | Docker Compose | one file, environment overrides |
| Frontend | Next.js + TS + Tailwind + shadcn/ui + Lightweight Charts | |

**Versions are pinned at M0** in `go.mod`, the compose images and `package.json`, after
verifying what is current — not asserted from memory.

---

## 7. Repository Layout

See `../CLAUDE.md` §6 for the full tree with module-to-decision mapping.
Directories are created when the module is written, not in advance.

---

## 8. Milestones

| # | Deliverable | Exit criteria |
|---|---|---|
| **M0** | Skeleton + tenancy foundation | `compose up` → `/healthz`; goose migrate; sqlc generate; OTel trace visible; two DB roles; `tenancy.InTx` wrapper; accounts/sessions/invites; **tenant isolation test green with the application-level `WHERE` deliberately removed** |
| **M1** | Asset/instrument registry + ledger + position engine — **no network** | Fixture replay: spot average cost + realized PnL correct; time-scoped alias resolution tested; idempotency, order-independence and rebuild-equality tests green |
| **M2** | Binance spot backfill | Real account history → ledger; idempotency holds across REST and WS paths; backfill resumes after interruption |
| **M3** | Portfolio + API + lineage | `GET /portfolio` correct; `GET /positions/{id}/lineage` opens a position down to its events |
| **M3.5** | Data quality + intra-venue transfers | Negative-balance / gap / unknown-symbol checks running; a spot ↔ futures transfer is not counted as a sale |
| **M4** | Market data + valuation | `price_ticks` populating; one `valuation_run` per response; USD price paths recorded; `freshness` populated; `GET /portfolio?at=` working |
| **M5** | Perpetuals + collateral | Funding, MMR, margin buffer, liquidation distance; one-way mode |
| **M6** | Strategy + risk + alerting | Strategy-level net delta; thresholds with hysteresis and cooldown; Telegram/webhook; SSE; dashboard v1 |
| **M7** | Reconciliation | Classified findings (`missing_event` / `duplicate` / `rounding` / `unsupported`) + a resync action |
| **M7.5** | Scenario shock | `POST /risk/scenario` projects equity and margin buffer under a price shock |
| **M8** | Bybit + cross-venue transfers | Two sources normalized correctly into one portfolio; withdrawals match deposits |

**M0 comes first because tenancy cannot be retrofitted.** Adding `account_id` and RLS to
a schema that already holds a real ledger means rebuilding every table.

**M1 finishes before any network code.** Debugging the engine against live data is the
most expensive path available.

---

## 9. Test Strategy

Detail in `ARCHITECTURE.md` §12. The four invariant tests, each guarding a property the
whole system rests on:

1. **Idempotency** — the same event set applied twice leaves state unchanged
2. **Order independence** — canonically sorted events give identical state regardless of
   ingest order (K21)
3. **Rebuild equality** — `positions` equals a fold from zero, exactly (L3)
4. **Tenant isolation** — verified with the primary defence deliberately removed, so the
   RLS backstop is proven on its own (K15)

Beyond those: golden fixture replay, and reconciliation tested against a Binance testnet
account. Engines are pure functions and must be unit-testable without a database (L4).

---

## 10. Questions Resolved Before Coding

The original draft listed six open questions. All are now decided:

| Question | Decision | Ref |
|---|---|---|
| Multi-user or single-user? | **Multi-user in V1.** App-level scoping + RLS backstop; invite-provisioned accounts, opaque revocable sessions | K15, K16 |
| Base currency: USD or USDT? | **USD.** Stablecoins are priced, not assumed at 1.00; the price path is recorded and `assumed_peg` is flagged | K17 |
| Fees paid in another asset (BNB)? | **Stay in their own asset.** Never folded into `avg_entry_price`; a separate realized-PnL component converted at `event_time` | K18 |
| Is a spot "position" just a balance? | **Average cost is tracked.** Otherwise spot PnL cannot be produced | K5 |
| How far back does backfill go? | **The account's full lifetime**, staged and resumable. A fixed window poisons cost basis permanently | K26 |
| Multiple Binance sub-accounts? | `account → integration → sub-account` exists in the model from V1, even before it appears in the UI | K15 |

**Also settled:** average-cost accounting stays in V1, but the ledger must remain
**lot-derivable** — a V2 tax-lot projection (FIFO/LIFO/HIFO, per-venue cost basis) will
be recomputed from events, not bolted onto the schema. Storing average cost as the only
truth is therefore forbidden (K5).

---

## 11. Working Setup

The agent operating manual is `../CLAUDE.md`, duplicated byte-for-byte as `../AGENTS.md`.
It carries the invariants (L1–L15), the role split, and the definition of done. It is
read at the start of every session.

- **Roles:** Claude implements and writes the tests, test-first. Codex is an independent
  second-eye reviewer and does not write production code. Because the implementer also
  writes the tests, that review is the compensating control, not a formality.
- **One module per session.** Cross-cutting refactors get their own session.
- `make generate` (sqlc), `make migrate`, `make test` are part of the build loop.
- Exchange payloads are recorded into `testdata/fixtures/` first, with credentials
  stripped by a checked-in redaction script. Agents work against fixtures, not the live
  API.
- Binance endpoint details — symbol requirements, `listenKey` lifetime, `positionRisk`
  version, weight costs — are verified against the official documentation before being
  coded. Never from memory: a wrong constant here produces plausible, wrong numbers.
