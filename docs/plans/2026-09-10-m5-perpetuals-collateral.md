# M5 — Perpetuals + Collateral Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the leveraged half of the account real — futures fills, funding, and the
collateral that decides how far the user is from being liquidated.

**Architecture:** Futures ingest reuses every shape spot already has (discovery → per-symbol
walk → normalizer → ledger → fold), and adds one thing the ledger cannot hold: a *snapshot*.
Margin balance, maintenance margin and liquidation price are the exchange's numbers about a
moment, not events, so they get their own table with their own freshness — and the risk
engine stays a pure function over what was captured (L4).

**Tech Stack:** Go 1.26 · pgx v5 · sqlc · goose · shopspring/decimal · Huma v2

**Spec:** `docs/PROJECT.md` §4 (collateral), §8 (M5 row) · `docs/DECISIONS.md` K5 K6 K18 K33
· `docs/BINANCE-API-NOTES.md` §8 (F14–F18) · `CLAUDE.md` L1 L3 L4 L9 L10 L11

---

## Context

M4 gave every asset a price and M3.5 stopped a transfer being read as a sale. What is still
missing is the reason this product exists for this user: **leverage**. A spot portfolio that
is wrong is embarrassing; a leveraged one that is wrong is a liquidation nobody saw coming.

The venue was verified before this plan was written (F14–F18), and it changed three things:

| Question | Answer, and what it changed |
|---|---|
| Where does maintenance margin come from? | **Not** `positionRisk` (F14). It is on `/fapi/v3/account`, so collateral is a snapshot of *that* endpoint while the liquidation price is a snapshot of `positionRisk` — two calls that must be captured as one act, or the buffer and the liquidation price describe two different accounts |
| Can M7.5 shock the maintenance margin? | Only with `leverageBracket` (F15). A shock changes notional and can cross into a higher `maintMarginRatio`, so scaling today's number is wrong. The brackets are captured here, in M5, because M7.5 is a pure function and cannot go fetch what it was not given |
| Does the futures user stream work like spot's? | No — the legacy URL has been **dead since 2026-04-23** (F17). `/private`, listenKey in the query string. A stream written from memory connects successfully and receives nothing, which looks exactly like a quiet account |

Two decisions taken before writing this plan:

| Question | Decision |
|---|---|
| Is `realizedPnl` from the venue ingested? | **No.** K5 computes realized PnL from the average-cost fold. The venue's number is a *reconciliation input* (M7), and ingesting it as well would be a second source of truth (L3) and a doubled number |
| Is the live user stream in M5? | **No — REST snapshot only.** The stream is a latency improvement over a snapshot loop, and M5's exit criteria are all about correctness. It lands in M6 beside SSE, where its value is visible. Recorded here so it is a decision rather than an omission |

---

## Global Constraints

Every task's requirements implicitly include this section.

- **L1** — money is `NUMERIC(38,18)` / `decimal.Decimal` / JSON **string**. A margin ratio and
  a maintenance margin rate are money paths too.
- **L3** — every projection survives a rebuild byte-identically. A snapshot is *not* a
  projection and does not get a rebuild test; it gets a freshness reason instead.
- **L4** — `risk` and `collateral` take their inputs. No DB handle, no clock, no network.
- **L9** — a fee belongs to the event that caused it. A futures commission rides on its fill.
- **L10** — one snapshot per response, named in `as_of`.
- **L11** — a stale snapshot is visible, never silently served.
- **K6** — liquidation price is read, never computed. Liquidation *distance* is computed.
- **Never** implement trade execution.

---

## Task 1: The futures market exists in the registry

**Files:**
- ~~Create: `backend/migrations/00022_futures_instruments.sql`~~ — unnecessary, see Step 3
- Modify: `backend/internal/exchange/binance/spot.go` (exchangeInfo for fapi)
- Test: `backend/internal/instrument/*_integration_test.go`

**Interfaces:**
- Produces: `instrument.MarketUSDM`; `binance.FuturesSymbols(ctx) ([]string, error)`.

- [x] **Step 1: Failing test — a USD-M instrument and a spot instrument with the same
  exchange symbol are two different instruments.** BTCUSDT spot and BTCUSDT perp share a
  ticker and are not the same thing; an alias table that cannot tell them apart attaches a
  perp fill to a spot position (L8, K10).
- [x] **Step 2: Failing test — the perp's settle asset is named.** A USD-M perp settles in
  USDT and that is what funding is paid in; an instrument that does not say so makes the
  funding fold guess.
- [x] **Step 3: ~~Migration~~ + exchangeInfo walk. Implement, run, commit.** No migration was
  needed: M1's `00005_instruments.sql` already has `kind = 'perp'`, a `settle_asset_id` that
  a CHECK requires on a perp, and `market` inside the alias key — and
  `TestSpotAndPerpAreDifferentInstruments` already existed. The step is done because the
  schema was right, not because it was written here.

---

## Task 2: The futures fill

**Files:**
- Create: `backend/internal/exchange/binance/futures.go`
- Create: `backend/testdata/fixtures/binance/user_trades_usdm.json` (documented)
- Test: `backend/internal/exchange/binance/futures_test.go`

**Interfaces:**
- Produces: `binance.NormalizeFuturesTrade(ctx, r, ic, raw) (ledger.Event, error)`,
  `binance.FuturesTradeID(symbol string, id int64) string`.

- [x] **Step 1: Failing test — one `userTrades` row becomes one TRADE**, identity
  `usdm:trade:<symbol>:<id>`, side from `side` rather than from `buyer`, commission on the
  event in `commissionAsset` (L9).
- [x] **Step 2: Failing test — `realizedPnl` is NOT stored.** The engine computes it (K5);
  storing the venue's copy would be a second source of truth and, folded, a doubled number.
  The test asserts the field is absent from the event and present in `raw` (L15).
- [x] **Step 3: Failing test — a `positionSide` other than `BOTH` is refused.** V1 is one-way
  mode. Hedge mode's two-sided position is a different fold, and silently averaging the two
  sides together produces a position that is flat when it is not.
- [x] **Step 4: Implement. Commit.**

---

## Task 3: Funding is a cash flow with no price

**Files:**
- Create: `backend/internal/exchange/binance/income.go`
- Create: `backend/testdata/fixtures/binance/income_usdm.json` (documented)
- Test: `backend/internal/exchange/binance/income_test.go`,
  `backend/internal/balance/balance_test.go`

**Interfaces:**
- Produces: `binance.NormalizeIncome(ctx, r, ic, raw) (ledger.Event, error)`,
  `binance.ErrUnknownIncomeType`, `binance.ErrIncomeReportedElsewhere`.

- [ ] **Step 1: Failing test — `FUNDING_FEE` becomes a `FUNDING_PAYMENT`** whose signed
  amount is the venue's `income`, in the settle asset, identity
  `usdm:income:FUNDING_FEE:<tranId>` (F3, F16).
- [ ] **Step 2: Failing test — `TRANSFER` is skipped, and says why** (F12). The wallet
  endpoint already reported that movement in M3.5; folding it here moves the money twice.
  Skipped with a named error rather than silently, because "we chose not to" and "we forgot"
  must not look the same in a log.
- [ ] **Step 3: Failing test — `REALIZED_PNL` is skipped** for the same reason as Task 2
  Step 2, and `COMMISSION` is skipped because the fill already carries it (L9). Every skip
  is a named sentinel, and an unrecognized type is `ErrUnknownIncomeType` — the enum is not
  fully published (F16), so the whitelist is the design and not a shortcut.
- [ ] **Step 4: Failing test — funding moves the balance and never the average entry
  price.** It is a cash flow in the settle asset (K18): a position that paid funding did not
  become more expensive to have opened.
- [ ] **Step 5: Implement. Commit.**

---

## Task 4: The snapshot the ledger cannot hold

**Files:**
- Create: `backend/migrations/00023_collateral_snapshots.sql`
- Create: `backend/internal/collateral/collateral.go` (pure)
- Test: `backend/internal/collateral/collateral_test.go`

**Interfaces:**
- Produces: `collateral.Snapshot`, `collateral.PositionRisk`, `collateral.Bracket`,
  `collateral.Buffer(s Snapshot) decimal.Decimal`,
  `collateral.Distance(mark, liquidation decimal.Decimal) decimal.NullDecimal`,
  `collateral.MaintenanceAt(brackets []Bracket, notional decimal.Decimal) (decimal.Decimal, error)`.

- [ ] **Step 1: Failing test — the margin buffer is margin balance minus maintenance
  margin**, and it is negative when it is negative. A buffer clamped at zero hides the one
  state the user most needs to see.
- [ ] **Step 2: Failing test — liquidation distance is `|mark - liq| / mark`, and it is
  ABSENT rather than infinite when there is no liquidation price.** A flat position has no
  liquidation price, and rendering "∞" for "not applicable" teaches a reader to ignore the
  field on the day it says something.
- [ ] **Step 3: Failing test — maintenance margin at a shocked notional crosses brackets**
  (F15). `MaintenanceAt` walks the table: `notional * maintMarginRatio - cum`. A notional
  above every bracket is an error, not the last bracket — the table ends where the venue's
  own risk model ends.
- [ ] **Step 4: Failing test — a snapshot's two halves must share an instant.** The
  constructor refuses a `Snapshot` whose account call and position call are more than a
  tolerance apart (F14): a buffer and a liquidation price from different moments describe
  two different accounts.
- [ ] **Step 5: Implement the pure engine, then the migration. Commit.**

---

## Task 5: Capturing it

**Files:**
- Create: `backend/internal/worker/collateral.go`
- Modify: `backend/internal/exchange/binance/futures.go` (three REST calls)
- Test: `backend/internal/worker/*_integration_test.go`

- [ ] **Step 1: Failing test — one capture writes one snapshot row per integration**, with
  the account totals, every position's risk, and the brackets for the symbols held.
- [ ] **Step 2: Failing test — the capture is one act.** The account call and the
  positionRisk call happen without an await in between that could be an hour, and the stored
  `as_of` is the earlier of the two — the snapshot is only as fresh as its stalest half.
- [ ] **Step 3: Failing test — a failed capture leaves the previous snapshot in place and
  raises `collateral_stale`.** The previous number with a warning beats no number; a blank
  margin buffer during a market event is the worst possible moment to have nothing.
- [ ] **Step 4: Implement, wire into the supervisor, commit.**

---

## Task 6: The account can see how close it is

**Files:**
- Create: `backend/internal/httpapi/risk.go`
- Modify: `backend/internal/freshness/reasons.go`
- Test: `backend/internal/httpapi/risk_integration_test.go`

**Interfaces:**
- Produces: `GET /risk`, `GET /funding`.

- [ ] **Step 1: Failing test — `GET /risk` reports equity, margin buffer, maintenance
  margin, and per-position liquidation distance**, every number a string, from ONE snapshot
  named in `as_of` (L10).
- [ ] **Step 2: Failing test — a stale snapshot is served WITH `collateral_stale`, never
  silently** (L11), and a missing one is `collateral_unavailable` rather than a zero buffer.
  A zero margin buffer and an unknown one are opposite claims and must not render the same.
- [ ] **Step 3: Failing test — `GET /funding` sums funding per symbol over a window**, from
  the ledger, and agrees with the events it names.
- [ ] **Step 4: M5's exit criterion end to end**: a funded futures account with one perp
  position reports a liquidation distance that moves when the mark moves and a buffer that
  falls when the position grows. Update PROJECT.md §8 and DECISIONS.md.

---

## Verification

M5 is complete when all of the following hold, each with observed output:

| Exit criterion (PROJECT.md §8) | How it is proven |
|---|---|
| Funding ingested | Task 3 Steps 1 and 4 |
| MMR available, including at a shocked notional | Task 4 Step 3 |
| Margin buffer computed and signed | Task 4 Step 1, Task 6 Step 1 |
| Liquidation distance | Task 4 Step 2, Task 6 Step 4 |
| One-way mode only | Task 2 Step 3 |
| Nothing double-counted | Task 2 Step 2, Task 3 Steps 2–3 |
| A doubtful snapshot is visible | Task 5 Step 3, Task 6 Step 2 |

**Not in M5, by decision:** the live futures user data stream (M6, beside SSE — F17 records
what it will need); COIN-M (V2); hedge mode (V2); portfolio margin (V2); auto-resync (M7).
