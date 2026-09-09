# M4 — Market Data and Valuation · Implementation Plan

> **For agentic workers:** Steps use checkbox (`- [ ]`) syntax for tracking. TDD
> throughout: write the failing test, watch it fail for the right reason, then implement.

**Goal:** Give the portfolio its headline number. `price_ticks` populating from a public
stream, one `valuation_run` behind every total, the USD price path recorded per asset, and
`GET /portfolio?at=` answering from durable history.

**Architecture:** A public market-data feed (no credential) fills `price_ticks` at minute
resolution. A pure pricing graph walks `asset → … → USD` and returns the path it used. The
worker produces valuation runs on a ticker; the API reads the latest completed one, so two
readers of the same instant see the same total.

**Spec:** `docs/PROJECT.md` §8 (M4 row), `docs/ARCHITECTURE.md` §5 §7,
`docs/DECISIONS.md` K7 K11 K17 K23 K28, `CLAUDE.md` L1 L4 L10 L11

---

## Context

M3 ships a portfolio with **no total**. Realized PnL on BTC-USDT is denominated in USDT
and on ETH-BTC in BTC; adding them makes a number with no unit, so the response carries
subtotals per quote asset and `valuation_unavailable` says why (K40). M4 is the milestone
that removes that reason.

**M4 is not blocked on what M2 is blocked on.** M2 cannot be finished without a real
read-only key, because its exit criterion is "real account history → ledger". Market data
is *public*: prices are the same for every account and need no signature. So this
milestone can be built and verified end to end today, and it is the reason to do it before
returning to M2.

Two things it must not quietly get wrong:

- **A stablecoin is not 1.00.** K17 is explicit: USDT and USDC are priced like any other
  asset, and a leg that falls back to a hard-coded 1.00 sets `assumed_peg`. A product
  selling correctness cannot have a blind spot shaped exactly like its worst tail risk.
- **A price is a claim with an age.** Serving a total built on a price from an hour ago
  without saying so is the confident-and-wrong failure L11 exists to reject.

## The five decisions taken before writing this plan

| Question | Decision |
|---|---|
| Where prices come from | Binance **public** market streams — no credential, so M4 is not blocked behind M2 |
| When a valuation run happens | The worker, on a ticker. The API reads the latest completed run and never writes one (K47) |
| What `?at=` reads | An ephemeral run rebuilt from `price_ticks`, reported identically but not stored (K48) |
| Whether Redis lands here | **No** — deferred to M6 (K49). K28 gives Redis two jobs; at M4 it would have one, and K28's own rule says remove it in that case |
| How USD is reached | A pure shortest-path walk over spot pairs to a designated peg asset, then one assumed hop, always recorded (K17) |

---

## Global Constraints

Every task's requirements implicitly include this section.

- **L1** — money is `NUMERIC(38,18)` / `decimal.Decimal` / JSON **string**. A price is
  money. So is a path's intermediate rate.
- **L4** — `internal/valuation` is pure: no DB handle, no clock, no network. Prices and
  time are inputs. This is what lets M7.5's scenario shock reuse it unchanged.
- **L10** — one valuation run per response. Two price sources producing two totals inside
  one response is forbidden.
- **L11** — `assumed_peg`, `price_stale` and `fee_price_missing` are reported, never
  swallowed.
- **L12** — every tenant read goes through `tenancy.InTx`. `price_ticks` is **reference
  data**, not tenant data: prices are the same for every account (the same exception
  `assets` and `instruments` already take).
- **CLAUDE.md §2** — every Binance detail is verified against the official docs. Never
  from memory, never from a blog post.

---

## Task 1: Verify the market-data surface, and write down what is true

This is the F1–F5 pattern from M2, and it comes first for the same reason: M2's plan was
rewritten by what the verification pass found, and finding it after the code was written
would have cost the code.

**Files:**
- Modify: `docs/BINANCE-API-NOTES.md` (new §6, "Market data")

**Questions the pass must answer, each with a quoted line and a date:**

- [x] **Step 1: Which public stream carries a last price for every spot symbol at once?**
  A per-symbol subscription for hundreds of symbols is a different design from one
  all-market stream, and the choice is not reversible cheaply. Candidates to check:
  the all-market ticker array streams and the individual symbol ticker streams. Record
  the exact stream name, its push interval, and its payload shape.
- [x] **Step 2: What is the connection lifetime and the ping/pong contract?**
  M2 learned the hard way that "a single connection is only valid for 24 hours" is a
  design input, not a footnote (F1). The market stream has its own answer and it must not
  be assumed to match the user-data one.
- [x] **Step 3: Is there a REST endpoint for a price snapshot, and what does it weigh?**
  Needed on start-up so a run can happen before the stream has pushed anything, and needed
  as the gap-fill after a disconnect. Record the weight — the shared per-IP budget (K24)
  is already spent by the account workers.
- [x] **Step 4: What historical price endpoint exists, and how far back?**
  `?at=` and fee pricing both need a price at a past instant. `price_ticks` only holds what
  we recorded from the moment M4 shipped, so a backfill of klines is the only way a
  historical question about last year gets a real answer rather than a gap.
- [x] **Step 5: Confirm the public streams need no signature and no key.**
  If this is wrong, the whole "M4 is not blocked on M2" premise collapses and the plan
  needs re-cutting before any code is written.

**Done when:** §6 exists, every claim carries a quote and a date, and anything that could
not be confirmed is listed as unverified rather than assumed — the same standard §5 holds.

---

## Task 2: `price_ticks` and the feed that fills it

**Files:**
- Create: `backend/migrations/00018_price_ticks.sql`
- Create: `backend/internal/marketdata/{client.go,stream.go,record.go}`
- Create: `backend/internal/store/queries/price.sql`
- Modify: `backend/cmd/worker/main.go`
- Test: `backend/internal/marketdata/*_test.go`, `*_integration_test.go`

**Schema shape:**

```
price_ticks
  instrument_id BIGINT NOT NULL REFERENCES instruments (id)
  ts            TIMESTAMPTZ NOT NULL          -- minute-truncated (K7)
  price         NUMERIC(38,18) NOT NULL
  source        TEXT NOT NULL
  PRIMARY KEY (instrument_id, ts)
  -- BRIN on ts: the table is append-mostly and time-ordered, which is the one access
  -- pattern BRIN is for, at a fraction of a btree's size (K7).
```

**No `account_id`, no RLS.** Prices are reference data — BTCUSDT is BTCUSDT for every
account — the same deliberate exception `assets` and `instruments` take (00004). What
protects it is privilege: the app role reads and does not write, so a bug in the API path
cannot invent a price. Only the worker writes it, as it is the only writer of everything
else.

**The minute is the key, and that is the deduplication.** `PRIMARY KEY (instrument_id, ts)`
with `ON CONFLICT DO UPDATE` means a stream pushing every second writes one row per minute
and the last price in that minute wins. A tick table that grew per push would be a
different product's problem within a week.

**Steps:**
- [x] **Step 1: Failing test — a minute holds one row, and it is the last price in it.**
- [x] **Step 2: Failing test — the recorder truncates to the minute in UTC**, so two
  processes in different zones write the same key rather than two.
- [x] **Step 3: Migration + queries + `go tool sqlc generate`.**
- [x] **Step 4: The public client and stream**, built on the verified facts from Task 1.
  It reuses `internal/exchange/binance`'s dialling and backoff rather than growing a
  second WebSocket implementation — but it carries **no credential and no signer**, which
  is the property that keeps it out of the account path entirely.
- [x] **Step 5: A snapshot on start**, so the first valuation run does not have to wait for
  a stream push, and a gap-fill on reconnect for the same reason a user-data gap is
  replayed: nothing in the protocol says what happened while the connection was down.
- [x] **Step 6: Wire into `cmd/worker`** as one feed per process, not one per integration.
  Prices are not per account, and a feed per account would multiply an IP-wide budget by
  the number of users (K24).
- [x] **Step 7: Commit.**

---

## Task 3: The pricing graph — pure, and it shows its work

**Files:**
- Create: `backend/internal/valuation/{graph.go,path.go,peg.go}`
- Test: `backend/internal/valuation/*_test.go` (no Docker)

**Produces:**
- `valuation.Rates` — the prices available at one instant, keyed by instrument
- `valuation.PriceOf(asset int64, r Rates, pegs PegSet) (Priced, error)`
- `valuation.Priced{ USD decimal.Decimal; Path []Hop; AssumedPeg bool; OldestObservedAt time.Time }`

There is no USDT/USD market, so USD is reached by walking (K17):

```
BTC ──BTCUSDT──▶ USDT ──USDCUSDT──▶ USDC ──assumed 1.00──▶ USD
```

**Every hop is recorded, and the record is the feature.** `GET /positions/{id}/lineage`
already answers "which events produced this position"; with the path stored it answers
"which prices produced this number, from which source, observed when" — which is the
product claim applied to the half M3 could not reach.

**Design constraints, each with a reason:**

- **Pure (L4).** Rates and the peg set are inputs. This is what lets M7.5's scenario shock
  re-price a whole portfolio under a hypothetical without a database, and what keeps these
  tests Docker-free.
- **Shortest path, with a deterministic tie-break.** Two runs a second apart must pick the
  same route, or the total moves for a reason no user can see and no lineage explains.
  Fewest hops first, then a stable ordering — never map iteration order.
- **`AssumedPeg` is sticky along the path.** One assumed leg makes the whole answer
  assumed. A path that averaged its confidence would report the most important failure as
  a rounding difference.
- **`OldestObservedAt` is the age of the *worst* leg, not the average.** A BTC price from
  one second ago routed through a USDC rate from an hour ago is an hour-old number.

**Steps:**
- [x] **Step 1: Failing test — a direct pair prices in one hop** and records it.
- [x] **Step 2: Failing test — a two-hop path multiplies the legs** and records both.
- [x] **Step 3: Failing test — an inverted pair is used correctly.** If only `BTCUSDT`
  exists, pricing USDT in BTC divides rather than multiplies, and a sign error here is a
  total that is wrong by orders of magnitude while looking plausible.
- [x] **Step 4: Failing test — the peg hop sets `AssumedPeg` and is in the path.**
  A stablecoin with a real market must be **priced, not pegged**: the assumed hop is the
  last resort, and a test proves the real rate wins when one exists (K17).
- [x] **Step 5: Failing test — an asset with no route is an error, not zero.**
  Zero is a number a dashboard will happily add up. `unknown_symbol` / no-route must reach
  the caller as a refusal.
- [x] **Step 6: Failing test — the path is stable across runs** with the same inputs.
- [x] **Step 7: Implement, then commit.**

---

## Task 4: `valuation_runs` — one run behind every total

**Files:**
- Create: `backend/migrations/00019_valuation_runs.sql`
- Create: `backend/internal/valuation/run.go`
- Create: `backend/internal/store/queries/valuation.sql`
- Modify: `backend/internal/worker/supervisor.go` (or a process-level runner)
- Test: `backend/internal/valuation/run_integration_test.go`

**Schema (ARCHITECTURE.md §5):**

```
valuation_runs    id, as_of, numeraire='USD', price_source, assumed_peg, freshness
valuation_prices  run_id, asset_id, price_usd, path, source, observed_at
```

**The run is produced by the worker on a ticker; the API never writes one (K47).**

Three reasons, and the third is the one that decides it:

1. A run is a write, and the API process is read-only by design (ARCHITECTURE §10 rule 5).
2. A run per request means a row per request — an unbounded table whose growth is driven
   by traffic rather than by time.
3. Two users refreshing a dashboard a second apart would each mint their own run and see
   different totals. "Every screen shows a different total" is the exact failure K11 exists
   to design out, and doing it per-request would reintroduce it inside our own product.

**Steps:**
- [x] **Step 1: Failing test — a run records every asset it priced, with its path.**
- [x] **Step 2: Failing test — a run with any assumed leg sets `assumed_peg` on the run.**
- [x] **Step 3: Failing test — the latest completed run is what a reader gets**, and a run
  still being written is never half-visible: prices and the run row commit together.
- [x] **Step 4: Migration, queries, generate.**
- [x] **Step 5: The producer**, on the same lease-guarded pattern the fold uses (K38) —
  one process values, and it is the one that already holds the write.
- [x] **Step 6: Commit.**

---

## Task 5: The portfolio gets its total, and admits what it cost

**Files:**
- Modify: `backend/internal/portfolio/{portfolio.go,load.go,freshness.go}`
- Modify: `backend/internal/httpapi/portfolio.go`
- Modify: `backend/internal/freshness/freshness.go` (retire `valuation_unavailable`)
- Test: `backend/internal/portfolio/*_test.go`, `backend/internal/httpapi/*_integration_test.go`

**What appears:** `total_value_usd`, per-holding `market_value` and `unrealized_pnl`, and
balances valued in USD. The `subtotals_by_quote_asset` block **stays** — it is exact and
needs no price, and a reader reconciling against an exchange screen denominated in USDT
wants it.

**What is narrowed, not retired:** `valuation_unavailable`. `ARCHITECTURE.md` §5 says
"M3; M4 removes it", and that turns out to be half right — the reason still has one honest
job left, and this plan corrects the table rather than deleting a code path the correction
would have needed. See the last row below.

**What replaces it, and when:**

| Condition | Reason | Severity |
|---|---|---|
| A leg fell back to 1.00 | `assumed_peg` | info |
| The run's oldest leg is older than tolerance | `price_stale` | warn |
| A fee asset has no price at the event's `event_time` | `fee_price_missing` | warn |
| An asset held has no route to USD | `unknown_symbol` | error |
| No run has completed yet | `valuation_unavailable` — **narrowed to exactly this** | warn |

The last row is why "M4 removes it" is wrong. On a system whose feed has never connected
there genuinely is no valuation, and a fresh install that deleted this reason would report
a confident zero — which is the failure the reason was written to prevent, arriving through
the door marked "cleanup". `ARCHITECTURE.md` §5's table is corrected as part of this task.

**Steps:**
- [ ] **Step 1: Failing test — the total is the sum of the valued holdings**, and every
  number in the response is a JSON string (the `requireMoneyIsString` walker already in
  `httpapi` covers new fields for free).
- [ ] **Step 2: Failing test — an assumed peg reaches the response** as `assumed_peg`,
  info, and does not degrade an otherwise exact total below `degraded`.
- [ ] **Step 3: Failing test — a stale run is reported, not hidden.**
- [ ] **Step 4: Failing test — an unpriceable holding does not silently drop out of the
  total.** It is the difference between "your portfolio is worth X" and "your portfolio is
  worth X, minus the part we could not price", and only one of those is true.
- [ ] **Step 5: Implement, then commit.**

---

## Task 6: `?at=`, PnL, and the prices in lineage

**Files:**
- Create: `backend/internal/portfolio/history.go`
- Modify: `backend/internal/portfolio/lineage.go`, `backend/internal/httpapi/*.go`
- Test: as above

**`GET /portfolio?at=T` reads an ephemeral run rebuilt from `price_ticks` (K48).**
It is not stored, and it does not need to be: `price_ticks` is durable and the graph is
pure, so the same T yields the same answer forever. Persisting it would grow a table with
one row per curious click and add nothing an audit could not already reproduce.

The positions at T come from folding the ledger to T — the machinery
`GET /positions/{id}/lineage` already has, applied to every instrument rather than one.
The cost is the account's whole history per call, which is honest for M4 and is what
`position_snapshots` exists to fix (ARCHITECTURE §3). Its invariant,
`snapshot(T) + events(T, T'] == full_fold(T')`, is the same equality lineage already
checks — a snapshot is a cache, so adding one must change this endpoint's timing and never
its answer.

**Steps:**
- [ ] **Step 1: Failing test — `?at=` before the first event is an empty portfolio**, not
  today's portfolio with an old timestamp.
- [ ] **Step 2: Failing test — `?at=` inside a gap in `price_ticks` reports the gap**
  rather than reaching forward to the next price. Reaching forward is look-ahead bias, and
  in a risk product it is the bug that makes a backtest look brilliant.
- [ ] **Step 3: Failing test — the same T twice gives byte-identical answers.**
- [ ] **Step 4: `GET /pnl?from&to`** — realized from the ledger, unrealized from the run at
  each end.
- [ ] **Step 4b: `GET /portfolio/history?from&to&interval`** — `?at=` applied at each
  interval boundary. Listed in `PROJECT.md` §5 and not in M4's exit criteria, so it lands
  here only if Steps 1–4 are green and the fold at T is fast enough to run N times; if it
  is not, that is the measurement that justifies `position_snapshots` rather than a guess.
- [ ] **Step 5: Fill lineage's `prices` block**, which M3 shipped present and empty exactly
  so this could land without changing the shape a client parses.
- [ ] **Step 6: Commit.**

---

## Verification

M4 is complete when each of these is true, with observed output:

| Exit criterion (PROJECT.md §8) | How it is proven |
|---|---|
| `price_ticks` populating | The stack runs for ten minutes and the table holds one row per instrument per minute |
| One `valuation_run` per response | A response names its run id; two responses inside one run window name the same one |
| USD price paths recorded | `valuation_prices` holds a path per asset; the lineage endpoint renders it |
| `freshness` populated | `assumed_peg` appears whenever a peg hop was used, and the tests force each reason |
| `GET /portfolio?at=` working | A T in the past reproduces the portfolio as it stood, twice, identically |
| No `float64` on a money path (L1) | `TestGeneratedCodeContainsNoFloat64` + the response walker |
| Rebuild equality (L3) | `price_ticks` is an input, not a projection — but the valuation must be reproducible from it, which Step 3 of Task 6 asserts |

**Mutation testing, as always.** The three that matter most here, because each one is a
wrong number that looks right:

1. Invert a path leg (multiply where it should divide).
2. Drop `assumed_peg` from a path that used one.
3. Let an unpriceable holding fall out of the total silently.

**Not in M4, by decision:** Redis (K49 — deferred to M6, where SSE gives it a second job),
perp mark price and funding (M5), `position_snapshots` (a cache, added when `?at=` is
measurably slow rather than in anticipation), and transfer matching (M3.5's remaining half).
