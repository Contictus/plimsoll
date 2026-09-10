# M6 — Strategy + Risk + Alerting + Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the numbers into something that watches the account for you — grouped the way
the positions were actually put on, and loud only when being loud is correct.

**Architecture:** Three pure engines and one delivery path. `strategy` groups positions by
user input that no rebuild may erase (K30). `risk` is a pure function of
`(positions, prices, collateral)` producing exposure, leverage, concentration and net delta
per asset, at portfolio AND strategy level (K13). `alert` is a pure function of
`(rules, metrics, previous state, now)` producing firings, with hysteresis and cooldown so
that a metric hovering on a threshold produces one alert rather than fifty. The worker
evaluates on **completed valuation runs**, never per tick. SSE fans out over Postgres
`LISTEN/NOTIFY`, and the dashboard is the first thing in this repository a human looks at.

**Tech Stack:** Go 1.26 · pgx v5 · sqlc · goose · shopspring/decimal · Huma v2 ·
Next.js (App Router) · Caddy

**Spec:** `docs/PROJECT.md` §4 (risk metrics), §5 (API), §8 (M6 row) ·
`docs/ARCHITECTURE.md` §7 (realtime, market data → risk), §9 (risk) ·
`docs/DECISIONS.md` K11 K13 K23 K27 K28 K30 · `docs/BINANCE-API-NOTES.md` F17 ·
`CLAUDE.md` L1 L3 L4 L10 L11 L12 L13

---

## Context

Everything up to here answers questions the user asks. M6 is the first milestone that
answers a question **nobody asked** — it tells the user something while they are asleep. That
inverts the failure mode: until now the worst outcome was a wrong number on a screen someone
chose to open, and from here the worst outcome is a stream of alerts that trains the user to
ignore the one that mattered.

Two things in the docs already decide most of the design:

| Constraint | Where | What it forces |
|---|---|---|
| A delta-neutral basis trade must not read as "2× leveraged" | K13, ARCHITECTURE §9 | Risk is computed per strategy as well as per portfolio, and `net_delta_per_asset` lives inside the strategy. A system that cannot group the two legs alerts constantly and incorrectly |
| Alerts evaluate on completed valuation runs, not per tick | ARCHITECTURE §7 | Per-tick evaluation fires on prices that never entered a published total — alerting on a number the user was never shown |
| The strategy tag is user input | K30 | It cannot live on `positions`, which is dropped and rebuilt. It lives in `position_strategies`, and the rebuild test is what proves it |

Three decisions taken before writing this plan:

| Question | Decision |
|---|---|
| Redis for SSE fan-out (K28)? | **No, not yet.** K28 gives Redis two jobs: the latest-price hash and SSE fan-out. M4 put prices in Postgres and never needed the first. Adding a container for the second — on a single VPS with one `api` process — is operational surface bought for nothing. SSE fans out over `LISTEN/NOTIFY`, which is durable-free, needs no new service, and is replaced by Redis the day a second `api` replica exists. Recorded as K51 |
| Alert delivery: which channels in V1? | **Webhook and Telegram**, both behind one `Deliverer` interface, both retried by the same loop. Telegram because it is the user's phone without an app to build; webhook because it is the escape hatch that makes every other channel someone else's problem |
| Does the live futures user stream land here? | **Yes.** M5 deferred it on purpose: it is a latency improvement, and M5 was about correctness. Its value only appears next to SSE, and F17 already documents the URL that has been dead since 2026-04-23 |

---

## Global Constraints

Every task's requirements implicitly include this section.

- **L1** — money is `NUMERIC(38,18)` / `decimal.Decimal` / JSON **string**. A leverage
  ratio, a concentration fraction and an alert threshold are money paths too.
- **L3** — `positions` survives a rebuild byte-identically, and the strategy tag survives it
  as well, because it is not on the projection (K30).
- **L4** — `risk` and `alert` take their inputs. No DB handle, no clock, no network, no
  logger. `now` is a parameter, which is what makes hysteresis and cooldown testable without
  sleeping.
- **L10** — one valuation run per response, named in `as_of`. A risk number and the portfolio
  number beside it come from the same run or the response is lying about one of them.
- **L11** — every degradation is a `freshness` reason. An alert evaluated on a stale run says
  so.
- **L12** — every tenant table carries `account_id`, RLS enabled **and** forced.
- **L13** — a webhook URL and a Telegram bot token are credentials: encrypted at rest (K25),
  never in a log, a trace, an error or a test fixture.
- **Never** implement trade execution. An alert is a message, never an action.

---

## File Structure

```
backend/
  migrations/
    00023_strategies.sql            strategies + position_strategies      (K13, K30)
    00024_alert_rules.sql           alert_rules + alerts + alert_state    (M6)
    00025_alert_channels.sql        delivery channels, envelope-encrypted (K25, L13)
  internal/
    strategy/       tagging, the account's strategy list — I/O around user input
    risk/           the pure engine: exposure, leverage, concentration, net delta
    alert/          the pure evaluator (hysteresis, cooldown) + delivery
    events/         LISTEN/NOTIFY fan-out, the SSE bus                    (K51)
    httpapi/
      strategy.go   GET/POST /strategies · PUT /positions/{id}/strategy
      exposure.go   GET /exposure — portfolio and per strategy
      alerts.go     GET /alerts · GET|PUT /alert-rules
      stream.go     GET /stream/{portfolio,risk,positions}   (SSE)
    exchange/binance/
      futures_stream.go   /private user data stream, listenKey on fapi    (F17)
frontend/                 Next.js dashboard v1                            (K27)
deploy/
  compose.yaml            + the frontend service
  Caddyfile               / → frontend, /api/* → api
```

---

## Task 1: The strategy tag, and the rebuild that must not erase it

**Files:**
- Create: `backend/migrations/00023_strategies.sql`, `backend/internal/strategy/strategy.go`,
  `backend/internal/store/queries/strategies.sql`, `backend/internal/httpapi/strategy.go`
- Test: `backend/internal/strategy/strategy_integration_test.go`,
  `backend/internal/httpapi/strategy_integration_test.go`

**Interfaces:**
- Produces: `strategy.Create(ctx, q, accountID, name, kind) (uuid.UUID, error)`,
  `strategy.List(ctx, q, accountID) ([]Strategy, error)`,
  `strategy.Assign(ctx, db, accountID, integrationID, instrumentID, strategyID) error`,
  `strategy.Of(ctx, q, accountID) (map[PositionKey]Strategy, error)`;
  routes `GET /strategies`, `POST /strategies`, `PUT /positions/{id}/strategy`.

- [ ] **Step 1: Failing test — a tag survives a projection rebuild.** Tag a folded position,
  drop and rebuild `positions` through the existing rebuild path, and read the tag back. This
  is K30's whole claim, and it is the one test that fails loudly if the tag is ever moved onto
  the projection: a tag stored there is erased by the rebuild, and the rebuild-equality test
  still passes because both sides are equally empty.
- [ ] **Step 2: Failing test — a tag cannot be attached to another account's position.**
  Written with the application-level `WHERE` deliberately absent, the way M0's isolation test
  is, so it proves RLS and not the query (L12).
- [ ] **Step 3: Failing test — one position has at most one strategy** (K13's V1 scope), and
  re-assigning replaces rather than accumulates.
- [ ] **Step 4: Migration, engine, endpoints. Commit.**

---

## Task 2: The risk engine, and the basis trade that must not read as 2× leveraged

**Files:**
- Create: `backend/internal/risk/risk.go`, `backend/internal/risk/risk_test.go`
- Modify: `backend/internal/portfolio/risk.go` (feed the engine what it already reads)

**Interfaces:**
- Produces (all pure, L4):
  `risk.Metrics{Equity, GrossExposure, NetExposure, Leverage, Concentration, NetDelta}`,
  `risk.Compute(in Input) Report` where
  `Input{Positions []Position, Prices map[string]decimal.Decimal, Collateral []Snapshot, Strategies map[PositionKey]string}`,
  and `Report{Portfolio Metrics, ByStrategy map[string]Metrics, Unpriced []string}`.

- [ ] **Step 1: Failing test — THE BASIS TRADE.** A BTC spot long of +$50,000 and a BTC perp
  short of −$50,000, both tagged into one strategy. The strategy's `net_delta_per_asset` for
  BTC is ~0 and its leverage is not 2×. The same two positions untagged report gross $100k
  against $50k equity — and the test asserts BOTH, because the wrong number is only wrong
  relative to the right one, and a test that saw only the right one would pass on an engine
  that ignored strategies entirely.
- [ ] **Step 2: Failing test — an unpriced asset is named, never counted as zero.** A position
  whose asset has no price in the run does not silently contribute 0 to gross exposure; it
  appears in `Unpriced` and the response says so (L11). Zero exposure and unknown exposure are
  opposite claims — the same rule as K50's margin buffer.
- [ ] **Step 3: Failing test — leverage with zero equity is undefined, not infinite.**
  `decimal.NullDecimal`, never a division that panics and never a very large number that reads
  as a real measurement.
- [ ] **Step 4: Failing test — concentration is per asset and sums to 1** across a priced
  portfolio, so a single-asset account reads 100% rather than an arbitrary top-N slice.
- [ ] **Step 5: Implement, mutation-test the aggregation, commit.**

---

## Task 3: `GET /exposure`, and strategy-level risk inside `GET /risk`

**Files:**
- Create: `backend/internal/httpapi/exposure.go`,
  `backend/internal/httpapi/exposure_integration_test.go`
- Modify: `backend/internal/portfolio/risk.go`, `backend/internal/httpapi/risk.go`

**Interfaces:**
- Produces: `GET /exposure`; `GET /risk` gains `by_strategy`.

- [ ] **Step 1: Failing test — `/exposure` and `/portfolio` agree**, because both come from
  one valuation run named in `as_of` (L10, K11). Two endpoints disagreeing about the same
  total is the failure this law exists to design out, and it is only observable across two
  endpoints — which is why the test lives here and not in the engine.
- [ ] **Step 2: Failing test — the basis trade through HTTP**: the strategy's net delta is ~0
  in the response body, every number a string.
- [ ] **Step 3: Failing test — a valuation run that never completed yields no exposure
  total**, with `valuation_unavailable`, rather than a confident zero.
- [ ] **Step 4: Implement, commit.**

---

## Task 4: The alert evaluator — hysteresis and cooldown, both pure

**Files:**
- Create: `backend/migrations/00024_alert_rules.sql`, `backend/internal/alert/alert.go`,
  `backend/internal/alert/alert_test.go`

**Interfaces:**
- Produces (pure, L4): `alert.Rule{ID, Metric, Scope, Comparator, Trigger, Clear, Cooldown}`,
  `alert.State{RuleID, Firing bool, Since, LastFiredAt}`,
  `alert.Evaluate(rules []Rule, values map[Key]decimal.Decimal, state map[uuid.UUID]State, now time.Time) ([]Firing, []State)`.

- [ ] **Step 1: Failing test — THE FLAPPING TEST.** A metric that crosses the trigger, falls
  back a hair, and crosses again — twenty times — produces **one** firing. Hysteresis means
  the clear threshold is a separate number from the trigger threshold, not the same number
  approached from the other side. A user who is messaged twenty times learns to silence the
  channel, and then the alerting system has negative value.
- [ ] **Step 2: Failing test — cooldown suppresses a re-fire but never a first fire.** Two
  rules on one metric, one of them freshly cooled down, and only the other fires. A cooldown
  that swallowed a first fire would be a silence indistinguishable from safety.
- [ ] **Step 3: Failing test — clearing is reported, not just firing.** A rule that has fired
  and then recovers emits a resolution, because an alert with no resolution leaves the user
  staring at a red row for a condition that ended hours ago.
- [ ] **Step 4: Failing test — a rule on a metric with no value does not fire.** Unknown is
  not "below the threshold" (L11, and the same rule as K50). It is reported as
  `metric_unavailable` on the rule rather than evaluated.
- [ ] **Step 5: Failing test — the evaluator is deterministic and clock-free**: the same
  inputs with the same `now` produce identical output, twice.
- [ ] **Step 6: Migration, implement, mutation-test the hysteresis boundaries, commit.**

---

## Task 5: Delivery, and the worker loop that evaluates on completed runs

**Files:**
- Create: `backend/migrations/00025_alert_channels.sql`,
  `backend/internal/alert/delivery.go`, `backend/internal/alert/runner.go`,
  `backend/internal/httpapi/alerts.go`
- Modify: `backend/cmd/worker/main.go`
- Test: `backend/internal/alert/delivery_test.go`,
  `backend/internal/alert/runner_integration_test.go`,
  `backend/internal/httpapi/alerts_integration_test.go`

**Interfaces:**
- Produces: `alert.Deliverer` interface with `Webhook` and `Telegram` implementations;
  `alert.Run(ctx, deps) error` — one evaluation pass over completed valuation runs;
  routes `GET /alerts`, `GET /alert-rules`, `PUT /alert-rules/{id}`.

- [ ] **Step 1: Failing test — a bot token never reaches a log, an error or a stored alert.**
  The delivery failure path is the dangerous one: the natural way to write it puts the request
  URL in the error, and the Telegram URL *contains the token* (L13). Assert on the error
  string, not on intent.
- [ ] **Step 2: Failing test — evaluation runs on a completed valuation run and not per
  tick** (ARCHITECTURE §7). A price written with no run completed produces no alert.
- [ ] **Step 3: Failing test — delivery is retried and recorded, and a failing channel does
  not lose the alert.** The alert row exists whether or not the message left the building, so
  `GET /alerts` is the record and the channel is the transport.
- [ ] **Step 4: Failing test — an alert evaluated on a stale run carries the run's freshness
  reasons**, so a message that says "leverage 4.1×" cannot be built from prices the user was
  told were stale, without saying so.
- [ ] **Step 5: Migration, implement, wire into `cmd/worker`, commit.**

---

## Task 6: SSE, over LISTEN/NOTIFY rather than a new container

**Files:**
- Create: `backend/internal/events/bus.go`, `backend/internal/httpapi/stream.go`
- Modify: `backend/internal/projection/*` and `backend/internal/valuation/*` (notify on
  advance), `deploy/Caddyfile` (SSE must not be buffered)
- Test: `backend/internal/events/bus_integration_test.go`,
  `backend/internal/httpapi/stream_integration_test.go`

**Interfaces:**
- Produces: `events.Publish(ctx, q, accountID, topic, payload) error`,
  `events.Subscribe(ctx, pool, accountID) (<-chan events.Message, error)`;
  routes `GET /stream/portfolio`, `GET /stream/risk`, `GET /stream/positions`.

- [ ] **Step 1: Failing test — a subscriber receives only its own account's events.** The
  channel name carries no tenant data and the payload is a *hint*, never the numbers: a client
  is told "portfolio changed" and re-reads through the authenticated endpoint. Pushing the
  numbers down a channel would put a second, unversioned copy of the API contract on the wire
  and a second place for a tenancy mistake to live.
- [ ] **Step 2: Failing test — a slow client is dropped, never allowed to block the
  publisher.** A bounded buffer per subscriber; overflow closes that connection and nothing
  else.
- [ ] **Step 3: Failing test — a disconnect frees the subscription**, and the process does not
  leak a goroutine per reconnect.
- [ ] **Step 4: Implement, add `flush_interval -1` to Caddy for the stream routes, commit.**

---

## Task 7: The live futures user stream (F17)

**Files:**
- Create: `backend/internal/exchange/binance/futures_stream.go`,
  `backend/testdata/fixtures/binance/user_stream_usdm.json`
- Modify: `backend/cmd/worker/supervise.go`
- Test: `backend/internal/exchange/binance/futures_stream_test.go`

**Interfaces:**
- Produces: `binance.NewFuturesStream(cfg FuturesStreamConfig) (*FuturesStream, error)`
  satisfying `worker.StreamSource`; `FuturesStreamURL = "wss://fstream.binance.com/private"`.

- [ ] **Step 1: Failing test — the URL and the listenKey path are the migrated ones (F17).**
  Assert the constant against the documented value, because the failure mode of getting this
  wrong is not an error: the legacy URL connects successfully and delivers nothing, which
  looks exactly like a quiet account.
- [ ] **Step 2: Failing test — `ORDER_TRADE_UPDATE` with a fill normalizes to the same
  `venue_event_id` the REST walk produces** (L5). If the two disagree, every futures fill is
  ingested twice and the position doubles — the exact failure L5 exists to prevent, arriving
  through a second door.
- [ ] **Step 3: Failing test — a non-fill order update is ignored rather than refused.** Most
  messages on this stream are orders being placed and cancelled; treating them as errors would
  turn a normal minute into a stopped supervisor.
- [ ] **Step 4: Failing test — the futures listenKey is kept alive on the futures host**, not
  the spot one, and its expiry is the documented one — verified against the page, never from
  memory.
- [ ] **Step 5: Implement, wire a second supervisor stream, commit.**

---

## Task 8: Dashboard v1

**Files:**
- Create: `frontend/` (Next.js App Router), `frontend/Dockerfile`
- Modify: `deploy/compose.yaml`, `deploy/Caddyfile`, `.env.example`, `Makefile`

**Interfaces:**
- Produces: `/` portfolio · `/positions/[id]` lineage · `/risk` · `/alerts` · `/strategies`,
  all same-origin behind Caddy (K27), authenticated by the existing HttpOnly cookie.

- [ ] **Step 1: Pin the version from the registry, not from memory.** `npm view next version`
  and `npm view react version`; write the exact versions into `package.json`. A guessed major
  is a build that fails in a way that looks like the code.
- [ ] **Step 2: Scaffold, with no auth code of its own.** The session cookie is HttpOnly and
  same-origin: the browser attaches it, and there is no token in JavaScript to store, refresh
  or leak (K16, K27). A 401 from any read is a redirect to the login page and nothing more.
- [ ] **Step 3: The freshness banner, before any number is rendered.** `freshness.status` is
  the first component built, and every page renders it. A dashboard that shows totals without
  showing what they are missing is precisely the product this repository argues against (L11),
  and building it last guarantees it is bolted on.
- [ ] **Step 4: Every number as text, never parsed into a JavaScript number.** `1e21` and
  `0.1 + 0.2` are the two ways a correct backend becomes a wrong screen. Strings in, strings
  formatted for display, no `parseFloat` anywhere in the tree — asserted by a test that greps
  the source, so it cannot be reintroduced quietly (L1).
- [ ] **Step 5: The risk page, which is what this user opens.** Margin buffer, liquidation
  distance per position, leverage per strategy, and the alert list — with `collateral_stale`
  and `collateral_unavailable` rendered as *different* states, never both as a dash.
- [ ] **Step 6: Live updates through the SSE bus** — a hint arrives, the page re-reads.
- [ ] **Step 7: Compose service, Caddy route, commit.**

---

## Verification

M6 is complete when all of the following are true, each with observed output:

| Exit criterion (PROJECT.md §8) | How it is proven |
|---|---|
| Strategy-level net delta | Task 2's basis-trade test, and Task 3's through HTTP |
| The tag survives a rebuild (K30) | Task 1 Step 1 |
| Thresholds with hysteresis and cooldown | Task 4's flapping test: twenty crossings, one alert |
| Telegram / webhook | Task 5, with the token-redaction test as the gate |
| SSE | Task 6, one account's events reaching only that account |
| Dashboard v1 | The stack up, the risk page rendering a margin buffer and a liquidation distance |
| No `float64` on a money path (L1) | `make lint` + the frontend's no-`parseFloat` test |
| Every response carries `as_of` and `freshness` | The existing envelope tests, extended to the new endpoints |

**Not in M6, by decision:** reconciliation (M7), scenario shock (M7.5), Bybit and cross-venue
transfer matching (M8), Redis (K51 — the day a second `api` replica exists), portfolio-level
strategy splitting of one instrument (K13 names this V2).
