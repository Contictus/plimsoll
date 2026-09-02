# Decision Register

Every architectural decision, why it was made, and what it costs. Referenced by ID
throughout the codebase and the other documents. K1–K14 come from the original project
draft; K15–K28 were added after the multi-user / correctness review.

A decision is only listed here if reversing it later would be expensive. Preferences
that can change freely do not belong in this file.

**Status legend:** `active` · `extended by Kn` · `superseded by Kn`

---

## Foundation

### K1 — The ledger is the only source of truth · `active`
`ledger_events` is append-only. Position, portfolio, PnL and risk are pure folds over
it. No engine keeps its own durable "truth"; what it keeps is a reproducible projection.

**Consequence:** the `positions` table is a cache. It must be droppable and rebuildable
from the ledger with an identical result, and there is a test enforcing that. This is
what makes historical reconstruction possible and what lets us recover from a
calculation bug instead of living with corrupted state.

### K2 — Bitemporal time · `active`
Every event carries two timestamps:
- `event_time` — what the exchange says happened, and the **only** time calculations use
- `ingested_at` — when we saw it, used for gap analysis, late-arrival detection, debugging

**Consequence:** "what was my portfolio at 2026-08-25T14:00Z" is a query over
`event_time <= T`, not a stored snapshot.

### K3 — Idempotent ingestion · `extended by K19`
REST backfill and the WebSocket stream deliver the same event. Inserting the same event
twice must produce the same ledger, and this is a tested invariant, not an intention.

**Original key was `UNIQUE (integration_id, source, external_id)` — that was wrong.**
See K19: including `source` in the key defeats the exact deduplication it was meant to
provide, because REST and WS report different `source` values for the same trade.

### K4 — Money representation · `active`
Postgres `NUMERIC(38, 18)`; Go `shopspring/decimal` mapped via sqlc `overrides`; JSON
**string**. `float64` never carries money, quantity, price, or fee — including during
serialization.

**Why 38/18:** 18 fractional digits covers wei-scale precision; 20 integer digits covers
any plausible notional. Binary floating point cannot represent `0.1` exactly, and in a
system whose entire claim is correctness, an off-by-a-satoshi total is a product failure.

### K5 — Exchange-style average cost · `active`
This is trading-position PnL, not tax accounting. Key: `(integration_id, instrument_id)`.
One-way mode only. A position can flip long → short in a single trade: realized PnL is
computed on the closed quantity, and the remainder opens in the new direction with a new
average entry.

**Constraint:** average cost may never be stored as the *only* truth. The ledger must
stay **lot-derivable** so a V2 tax-lot projection (FIFO/LIFO/HIFO, per-venue cost basis)
can be recomputed from events rather than bolted onto the schema. See K12 in
`COMPETITIVE-ANALYSIS.md` §5 (G12) for why this is a regulatory constraint, not a nicety.

### K6 — Liquidation price is read, not computed · `active`
We do not calculate it ourselves — it depends on margin-tier tables, cross vs isolated
margin, and wallet balance interactions that drift without notice. The exchange's
liquidation price is stored in the `position_risk` snapshot; **liquidation distance** is
what we compute, from the mark price.

**Trade-off:** we inherit the exchange's number, including its staleness. Acceptable,
because a confidently wrong liquidation price is far more dangerous than a slightly late
correct one.

### K7 — Price history is durable · `extended by K28`
Redis holds only the latest price. Historical valuation reads `price_ticks` in Postgres —
minute-resolution mark price is sufficient, not every tick. BRIN index on `ts`.
No TimescaleDB or ClickHouse in V1.

### K8 — Modular monolith, no broker · `active`
`cmd/api` + `cmd/worker`. Modules talk through Go interfaces, not the network. Kafka/NATS
are not added until a real need appears.

**Why:** the hard problems here are correctness problems, not throughput problems. A
message broker would add operational surface and distributed-ordering bugs to a system
whose value proposition is that its numbers are right.

### K9 — Credential security · `extended by K25`
API keys are **read-only**: withdrawal disabled, trading disabled. Permission is verified
when the connection is established, and an over-permissioned key is rejected rather than
accepted with a warning. Encrypted at rest with AES-256-GCM. Plaintext never reaches a
log.

### K10 — Asset is not Instrument · `extended by K22`
Most valuation errors are not price errors, they are **identity errors**: ticker
collisions, wrapped/bridged assets, the same symbol used in both spot and derivative
markets. `assets` (BTC, ETH, USDT — the canonical thing) and `instruments`
(BTC-USDT-PERP — the tradable thing) are separate tables. An exchange symbol is never a
key; it is always resolved through an alias table.

**Concrete case:** on Binance, spot `BTCUSDT` and perpetual `BTCUSDT` are the same
string and different instruments. Without this split the positions merge and every
downstream number is wrong.

### K11 — A single valuation policy · `extended by K17, K23`
Every portfolio and risk response is produced from one `valuation_run` carrying `as_of`,
`price_source`, and staleness. Producing two different totals from two different price
sources in one response is forbidden. Price-source priority is defined per instrument
(perp → exchange mark price, spot → exchange last/index) and the fallback chain is
recorded.

**Why:** "every screen shows a different total" is the failure that makes users believe
a product is lying to them. See `COMPETITIVE-ANALYSIS.md` §3.3.

### K12 — A transfer is not a sale · `active`
An asset withdrawn from one venue and deposited into another is **one transfer**.
Unmatched, the system reads it as a disposal plus an acquisition and PnL collapses.
`transfer_links` joins the two ledger events. Matching heuristic: same asset + amount
(within fee tolerance) + time window + txid when available. Unmatched pairs go to a
queue the user can resolve manually.

V1 covers intra-venue transfers (spot ↔ futures); M8 covers cross-venue.

**Why it matters:** this is the entire industry's number-one support topic.

### K13 — Strategy is a first-class dimension · `active`
Positions and events carry an optional `strategy` tag. A delta-neutral basis trade
(spot long + perp short) that is not grouped under one strategy gets reported as
"2× leveraged, risky" when its net delta is ~0 — and the system then generates false
alerts forever. Exposure and leverage are computed at both portfolio and strategy level.

**V1 scope:** strategy is assigned at the **position** level — one strategy per position.
Splitting a single instrument's position across strategies is V2.

### K14 — Data quality is a visible feature · `active`
Every product shows numbers; our difference is being able to show *why* the number is
right. Continuously running checks, served by their own endpoint:

- **Negative balance** — the ledger implies selling more than was ever held ⇒ a missing event
- Sequence / WebSocket gap, missed keepalive
- Unknown asset or unresolvable symbol
- Price gap (the mark price stream stopped)
- Unmatched transfer
- Missing price for a fee asset at `event_time`
- Exchange clock skew beyond tolerance

---

## Multi-tenancy and access

### K15 — Multi-user from day one, with RLS as a backstop · `active`
The system serves multiple users in V1. Every tenant table carries `account_id`
**denormalized onto the row** — not reachable only through a join — because RLS policies
must evaluate against a column to stay fast and simple.

Two independent defences:
1. **Primary:** every query is scoped by `account_id` at the application layer.
2. **Backstop:** `ROW LEVEL SECURITY` enabled *and* `FORCE`d on all tenant tables; each
   transaction opens with `SET LOCAL app.account_id`.

Cross-tenant mixing is additionally impossible at the schema level via a composite
foreign key `(account_id, integration_id) → integrations(account_id, id)`.

**Two traps this decision must respect:**
- A table's **owner bypasses RLS**. Migrations run as the owner role; the application
  connects as a separate restricted role. `FORCE ROW LEVEL SECURITY` closes the rest.
- `SET LOCAL` only lives inside a transaction, so **every** tenant read runs in one.
  That cost is accepted deliberately.

**Cost:** all reads are transactional; a `tenancy` wrapper is mandatory boilerplate.
**Bought:** a forgotten `WHERE` returns an empty result instead of another user's
portfolio.

### K16 — Invite-based accounts, opaque revocable sessions · `active`
No open sign-up in V1. Accounts are provisioned by an administrator, which removes email
verification, password reset, bot protection and mail infrastructure from V1 entirely.

- Password hashing: **argon2id** (`x/crypto/argon2`)
- Session: 32 bytes from `crypto/rand`, stored in Postgres as a SHA-256 hash, delivered
  as an `HttpOnly; Secure; SameSite=Lax` cookie

**Explicitly not JWT.** A JWT cannot be revoked before it expires. "I deleted my API key
but the session lived another 15 minutes" is not acceptable in a system holding exchange
credentials.

---

## Correctness fixes found in review

These four correct defects in the original draft. Each would have required rebuilding
the ledger if discovered after implementation.

### K17 — USD numeraire, stablecoins are priced · `active`
All portfolio totals and equity are computed in **USD**. USDT and USDC are *not* assumed
to be worth 1.00 — they are priced like any other asset.

Because no direct USDT/USD pair exists, valuation walks a **price path**
(`asset → quote asset → … → USD`). The `valuation_run` records the path actually used
and sets `assumed_peg = true` whenever a leg fell back to a hard-coded 1.00.

**Why not USDT as the base:** it matches the exchange screen and simplifies
reconciliation, but if USDT depegs the system reports "everything is normal". A product
selling correctness cannot have a blind spot shaped exactly like its worst tail risk.

**Cost:** an extra price source and a pricing graph in `valuation`.

### K18 — A fee stays in its own asset · `active`
When commission is paid in BNB, the ledger keeps it as-is (`fee` + `fee_asset`).
`avg_entry_price` remains the clean execution price, so it matches what the exchange
displays and reconciliation stays free of unexplained deltas.

Fees are a separate realized-PnL component, converted to USD at the fee event's
`event_time`. If no price exists for the fee asset at that time, a data-quality finding
is raised (K14) rather than the fee being silently dropped.

**Also resolved:** the original draft defined a fee both as a column on
`ledger_events` and as a `FEE` event type — writing both double-counts it. The rule is
now: a fee belongs to the event that caused it; the standalone `FEE` type exists only
for fees with no parent event.

### K19 — Event identity is source-independent · `supersedes part of K3`
```
UNIQUE (integration_id, venue_event_id)     -- e.g. spot:trade:BTCUSDT:12345678
```
`source` (`binance_spot_rest`, `binance_usdm_ws`, …) is recorded as metadata — which
ingest path saw the event first — and is **never** part of identity.

**The defect this fixes:** K3's stated purpose was that REST and WS delivering the same
trade must deduplicate. But its key included `source`, and REST and WS carry *different*
source values, so the constraint never fired. Both rows would insert and every position
would be exactly double. Correct on paper, broken in the schema.

### K20 — One writer per integration; never project on global `seq` · `active`
`seq BIGSERIAL` cannot order commits. Sequence values are handed out *before* commit, so
a reader can see `seq=100` committed while `seq=99` is still uncommitted. A `last_seq`
projection cursor skips that event, and because the ledger is append-only (L2) it is
skipped forever.

**Resolution:**
- Projections advance **per `integration_id`**, never over the global sequence.
- Exactly one worker owns an integration at a time, held by a Postgres lease row
  (`owner_id`, `heartbeat_at`, `expires_at`), which makes the per-integration stream
  single-writer.
- `seq` is retained for debugging and lineage display only.

### K21 — Canonical event ordering · `active`
```
ORDER BY event_time, venue_sequence, venue_event_id
```
Ordering by `event_time` alone is undefined: the fills of a single order share one
millisecond timestamp on Binance. Under an undefined order, `avg_entry_price` depends on
ingest order and the order-independence test (`docs/PROJECT.md` §10) becomes flaky in a
way that looks like a heisenbug rather than a schema gap.

`venue_sequence` stores the exchange's own trade/event id as a sortable value and is a
required column, not an optional one.

### K22 — Aliases are time-scoped · `extends K10`
`asset_aliases` and `instrument_aliases` carry `valid_from` / `valid_to`, with an
exclusion constraint preventing overlapping windows for the same external symbol.
Resolution always happens **at the event's `event_time`**, never with today's mapping.

**Why:** exchanges recycle symbols after a delisting. With an untimed alias table, a 2024
trade resolves to the 2026 instrument — a correct quantity attached to the wrong
identity. This is precisely the failure mode described in `COMPETITIVE-ANALYSIS.md`
§3.1, where an identity error presents itself as a price error and is debugged in the
wrong place for weeks.

### K23 — Structured freshness replaces the `stale` boolean · `extends K11`
A single `stale: true` flattens unrelated conditions — WS gap, stale price, incomplete
backfill, assumed peg, open reconciliation finding — into one bit that tells the user
nothing actionable.

Every response instead carries:
```
freshness: {
  status:  ok | degraded | unreliable,
  reasons: [ { code, severity, detail, since } ]
}
```
For a product whose pitch is "I can show you why my numbers are correct", this field is
the pitch made concrete. It is API surface, not diagnostics.

---

## Operational

### K24 — Two-tier rate limiting · `active`
Binance enforces weight budgets **per API key** *and* separate limits **per IP**, and
caps WebSocket connections per IP. On a single VPS every user shares one IP, so
per-key accounting alone leaves the key budgets healthy while the IP gets banned.

- **Tier 1:** per-integration weight budget
- **Tier 2:** a process-wide shared per-IP budget every outbound call passes through
- WebSocket connections are handed out by a quota-aware connection manager

Implemented with `x/time/rate`; `WaitN(ctx, weight)` models variable endpoint cost
directly.

**Work priority** is fixed and not negotiable at runtime:
`realtime > reconciliation > backfill`. Backfill is chunked and interleaved so one
user's full-history import cannot starve another user's live stream.

### K25 — Envelope encryption for credentials · `extends K9`
Each account has its own DEK; the DEK is wrapped by a master KEK. Rows carry
`key_version`.

**Bought:** a single account's key can be rotated without touching everyone else's, and
a leak has a bounded blast radius. Encrypting everything directly with one master key
has neither property.

The master key comes from a `KeyProvider` interface. V1 implements it from an env file
(`0600`, outside the repo); moving to KMS is one new implementation, not a migration.

### K26 — Full-history staged backfill · `active`
Backfill goes back to the account's first trade, not a fixed window.

**Why not one year:** an asset acquired outside the window and sold inside it makes the
ledger produce a negative balance, which trips the K14 check permanently. Working around
that requires an opening-balance concept — an entire synthetic-event subsystem invented
purely to paper over missing history, and one that permanently poisons cost basis.

Backfill is chunked with persisted progress so it resumes where it stopped. Until it
completes, the portfolio is served with `backfill_incomplete` in `freshness` (K23) —
visible, never silent.

### K27 — Single VPS, same-origin, Caddy · `active`
Docker Compose on one server; the same compose file locally and in production, differing
only by override. Caddy terminates TLS and serves the frontend and API **under one
origin**.

**Consequence, and the reason:** no CORS configuration exists, and the session token is
never reachable from JavaScript. A cross-origin setup would require CORS rules plus a
token storage strategy — two more chances to leak credentials, bought for nothing.

### K28 — Redis holds only rebuildable state · `extends K7`
Redis has exactly two jobs: the latest-price hash and SSE fan-out pub/sub. Losing Redis
degrades latency; it never loses data. Anything durable is in Postgres.

Reviewed for removal — Postgres `LISTEN/NOTIFY` could cover the fan-out — and kept,
because two genuine jobs justify the component. If it ever drops to one, remove it.

---

## Deliberately Out of Scope

| Not doing | Why |
|---|---|
| **Trade execution** | Ever. The API key permission will not allow it. Non-negotiable. |
| **Competing on integration count** | 1Token lists 72 exchanges and 4,224 DeFi protocols. That race is unwinnable and beside the point. We differentiate on **accuracy and risk depth**, not coverage. |
| Options, Greeks, VaR | Meaningless without options in scope. |
| Hedge mode (two-sided positions) | V1 is one-way mode only. |
| Multi-asset / portfolio margin | V2 at the earliest. |
| COIN-M futures | V2. |
| EVM wallets, Solana, DeFi, LP | A different ingestion domain entirely. |
| Tax-purpose FIFO/LIFO accounting | V2 — but the ledger stays lot-derivable from day one (K5). |
| CCXT as the exchange client | Its WebSocket support is paid, the Go port is transpiled rather than idiomatic, and user-data-stream normalization **is** this project's core domain work. Outsourcing it leaves CRUD behind. Its market-metadata and symbol-normalization model is still worth reading as reference. |
