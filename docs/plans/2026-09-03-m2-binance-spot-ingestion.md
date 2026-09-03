# M2 — Binance Spot Ingestion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps
> use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Connect a real read-only Binance spot key, import the account's entire trade
history into the ledger, and keep it current from the WebSocket stream — with the same
event landing once whichever path saw it first.

**Architecture:** `cmd/worker` claims a lease per integration and runs one supervisor
goroutine for it. The supervisor obtains credentials, verifies the key is read-only,
discovers which symbols the account ever traded, walks each one's history through a
resumable cursor, and subscribes to the live stream. Every outbound call passes a two-tier
rate limiter. Normalization turns exchange payloads into canonical events; the ledger
deduplicates on venue identity.

**Tech Stack:** Go 1.26 · `x/time/rate` · `nhooyr`-style `gorilla/websocket` (pinned at
task 7) · AES-256-GCM · PostgreSQL 18 · recorded fixtures

**Spec:** `docs/PROJECT.md` §8 (M2 row) §3, `docs/ARCHITECTURE.md` §1 §3–§5,
`docs/DECISIONS.md` K3 K9 K14 K19 K20 K23 K24 K25 K26, `docs/BINANCE-API-NOTES.md`,
`CLAUDE.md` L1 L5 L11 L13 L15

---

## Context

M1 shipped the ledger, the registries and the fold, all against fixtures. M2 is the first
network code in the project, and it is where a wrong assumption stops being cheap.

`docs/BINANCE-API-NOTES.md` was written before this plan, from the official documentation,
and five of its findings shape the work below. Three of them contradict what the ecosystem
still widely documents:

| | Finding | What it changes |
|---|---|---|
| F1 | Spot `listenKey` was removed; user data comes from the WebSocket API | Task 7 has no listenKey lifecycle at all. Futures still uses one — the two markets do not share a realtime path, which matters at M5, not here |
| F2 | Futures history stops at 3 months | Out of M2's scope (spot only), but the freshness reason it needs is added in task 8 rather than invented under pressure later |
| F3 | `tranId` is unique per `incomeType`, not globally | Confirms the `venue_event_id` shape `ARCHITECTURE.md` §3 already documents |
| F4 | No endpoint returns spot trades across symbols | Task 6 exists. Discovery is our problem, and the obvious solutions all have a hole in them |
| F5 | The 24-hour window binds `startTime`/`endTime`, not `fromId` | The backfill walks by trade id, not by time chunk — one weight-20 request per 1000 trades instead of one per day |

**The decision taken before writing this plan:**

| Question | Decision |
|---|---|
| Spot realtime needs Ed25519 (`session.logon`) or HMAC (`.signature`)? | **HMAC signature variant.** K25's credential model is unchanged and the user pastes one key/secret. Ed25519 migration becomes its own task if Binance removes the HMAC path |

**Scope:** spot only. USD-M futures ingestion, funding and `positionRisk` belong to M5;
this plan touches none of them, and no code here may quietly assume a single market.

---

## Global Constraints

Every task's requirements implicitly include this section.

- **L1** — money is `NUMERIC(38,18)` / `decimal.Decimal` / JSON string. Binance sends
  quantities and prices as **JSON strings**; they are parsed with
  `decimal.NewFromString` and never through `float64`, not even in a fixture helper.
- **L5** — `venue_event_id` is built from exchange fields only:
  `spot:trade:BTCUSDT:12345678`. Never from our clock, our counters, or the ingest path.
  REST and WS must produce byte-identical ids for the same trade — that is the property
  the whole milestone is judged on.
- **L11** — every degraded condition is a structured `freshness` reason, never silence.
- **L13** — no plaintext credential in a log, trace, error, fixture or test output. The
  recorded fixtures are redacted **before** they are written to disk, not after.
- **L15** — the exchange payload is stored verbatim in `raw`. Store JSON, not SBE: a JSON
  payload can be read six months from now by a human debugging a normalization bug.
- **Trade execution is never implemented.** The key is verified read-only and refused
  otherwise (K9). No order endpoint is wrapped, not even unused.
- **Fixtures before network** (`CLAUDE.md` §2). Record once, redact, then develop against
  the file. Never iterate against the live API.

---

## File Structure

```
backend/
  migrations/
    00008_integration_credentials.sql   credential columns on integrations (K25)
    00009_backfill_progress.sql         per-scope resumable cursor (K26)
    00010_integration_leases.sql        single-writer lease (K20)
  internal/
    crypto/envelope.go                  DEK/KEK envelope encryption, KeyProvider
    integration/credential.go           store + retrieve, never logs
    integration/connect.go              verify read-only, register, reject over-permissioned
    ratelimit/limiter.go                two-tier: per-integration weight + shared per-IP
    exchange/binance/
      client.go                         signed REST, weight accounting, 429/418 handling
      spot.go                           the six endpoints M2 uses
      normalize.go                      payload -> ledger.Event
      stream.go                         WS API user data subscription
    backfill/
      discover.go                       which symbols this account ever traded (F4)
      walk.go                           fromId cursor per symbol, resumable (F5, K26)
    worker/
      lease.go                          claim and heartbeat
      supervisor.go                     the per-integration state machine
  testdata/fixtures/binance/            recorded, redacted, committed
```

---

## Task 1: Envelope encryption, and a credential that cannot be printed

**Files:**
- Create: `backend/internal/crypto/envelope.go`
- Create: `backend/migrations/00008_integration_credentials.sql`
- Create: `backend/internal/integration/credential.go`
- Test: `backend/internal/crypto/envelope_test.go` (unit),
  `backend/internal/integration/credential_integration_test.go`

**Interfaces:**
- Produces:
  - `crypto.KeyProvider` — `MasterKey(ctx, version int) ([]byte, error)`
  - `crypto.EnvFileProvider` — reads `PLIMSOLL_MASTER_KEK`, the V1 implementation
  - `crypto.Seal(kp KeyProvider, plaintext []byte) (ciphertext, wrappedDEK []byte, version int, err error)`
  - `crypto.Open(kp KeyProvider, ciphertext, wrappedDEK []byte, version int) ([]byte, error)`
  - `integration.Credential{APIKey, APISecret auth.Secret}`
  - `integration.StoreCredential(ctx, q, accountID, integrationID, Credential) error`
  - `integration.LoadCredential(ctx, q, accountID, integrationID) (Credential, error)`

- [ ] **Step 1: Write the failing unit tests for the envelope**

Cases, each one a property that must hold rather than a line of coverage:
- a round trip returns the plaintext exactly
- two seals of the same plaintext produce different ciphertext (fresh DEK, fresh nonce)
- ciphertext with one flipped byte fails to open, and fails with an error rather than
  returning garbage (this is what GCM is for; assert it rather than trusting it)
- a wrapped DEK from account A cannot open account B's ciphertext
- `Open` with the wrong `version` fails rather than silently trying the current key
- the error returned on any failure contains **no** key material — assert on the message,
  because this is the path an operator will read at 2am

- [ ] **Step 2: Run and watch them fail.** Expected: `package .../internal/crypto is not in std`.

- [ ] **Step 3: Implement `envelope.go`.** AES-256-GCM for both layers. A fresh 32-byte DEK
      per seal; the DEK is wrapped with the master KEK; nonces are random and prefixed to
      their ciphertext. `key_version` travels on the row so a rotation is additive.

- [ ] **Step 4: Write the migration.** Add to `integrations`:
      `credential_ciphertext BYTEA`, `wrapped_dek BYTEA`, `key_version INT`,
      `credential_verified_at TIMESTAMPTZ`. All nullable — an integration exists before its
      credential does. Grant the app role `INSERT, UPDATE` on `integrations` (M1 gave it
      `SELECT` only, deliberately; this is the milestone that earns the write).

- [ ] **Step 5: Write the failing integration test.** Store a credential, load it back,
      and assert: it round-trips; a second account cannot load it (RLS, with the
      application `WHERE` deliberately absent); and `SELECT credential_ciphertext::text`
      contains neither the key nor the secret in readable form.

- [ ] **Step 6: Implement `credential.go`.** `Credential` fields are `auth.Secret`, so a
      careless `%v` or a `slog` call prints `REDACTED` (L13). No `String()` that reveals.

- [ ] **Step 7: Mutation-test the guards.** Remove the per-seal DEK and reuse one; remove
      the version check; log the credential struct with `%+v`. Each must fail a test.

- [ ] **Step 8: Commit.**

---

## Task 2: Read-only verification, and refusing a key that can trade

**Files:**
- Create: `backend/internal/integration/connect.go`
- Test: `backend/internal/integration/connect_test.go` (unit, against fixtures)

**Consumes:** task 1's credential storage.
**Produces:**
- `integration.Permissions{Reading, Withdrawals, SpotTrading, MarginTrading, Futures bool}`
- `integration.ErrOverPermissioned`
- `integration.Verify(ctx, c Client, cred Credential) (Permissions, error)`

K9 is unambiguous: an over-permissioned key is **rejected**, not accepted with a warning.
This task is small and it is the one a reviewer should read first.

- [ ] **Step 1: Record the fixture.** `GET /sapi/v1/account/apiRestrictions` against a real
      read-only key, redacted. Also record a hand-edited variant with
      `enableWithdrawals: true` — a fixture for a case we must never accept is worth more
      than one for the happy path.

- [ ] **Step 2: Write the failing tests.**
      - a read-only key verifies and returns `Reading: true` with everything else false
      - `enableWithdrawals: true` returns `ErrOverPermissioned`, and the error names the
        permission to remove — an error that says "rejected" without saying why makes the
        user disable the wrong thing
      - `enableSpotAndMarginTrading: true` likewise
      - **an unknown permission field that is `true` is also a rejection.** Binance adds
        permissions; defaulting an unrecognized one to harmless is how a trading-capable
        key gets accepted a year from now
      - a key with `enableReading: false` is rejected: it cannot do the job

- [ ] **Step 3: Run and watch them fail.**

- [ ] **Step 4: Implement.** Decode into a struct **plus** a `map[string]any` for the
      unknown-field check. Note in a comment that `enableFutures` is a read permission here
      rather than a trading one, and that this was verified against the response rather
      than assumed — M5 will need it true.

- [ ] **Step 5: Mutation-test.** Make the unknown-field check pass everything; make
      `enableWithdrawals` a warning instead of a rejection. Each must fail a test.

- [ ] **Step 6: Commit.**

---

## Task 3: The two-tier rate limiter

**Files:**
- Create: `backend/internal/ratelimit/limiter.go`
- Test: `backend/internal/ratelimit/limiter_test.go` (unit, clock injected)

**Produces:**
- `ratelimit.Limiter` with
  `Acquire(ctx context.Context, integrationID uuid.UUID, weight int) error`
- `ratelimit.Priority` (`PriorityRealtime` > `PriorityReconcile` > `PriorityBackfill`)
- `ratelimit.Penalize(retryAfter time.Duration)` — what a 429 does to the shared tier

K24 exists because **the limits are per IP, not per key** — confirmed in
`BINANCE-API-NOTES.md` §3. One server, many accounts, one budget. A per-key limiter alone
leaves every key healthy while the IP earns a ban of up to three days.

- [ ] **Step 1: Write the failing tests.** Time is injected (L4), so none of these sleep:
      - two integrations each inside their own budget still block each other once the
        shared IP budget is exhausted — **the test the tier exists for**
      - `Acquire` with a weight larger than the burst fails fast rather than blocking
        forever
      - a 429 with `Retry-After: 30` stops **every** integration for 30s, not just the one
        that earned it: the ban is on the IP
      - backfill yields to realtime — with the shared budget saturated, a realtime
        `Acquire` is served before a backfill one that was queued first
      - a cancelled context returns promptly and does not consume budget

- [ ] **Step 2: Run and watch them fail.**

- [ ] **Step 3: Implement** on `x/time/rate`. `WaitN(ctx, weight)` models variable endpoint
      cost directly, which is why the interface takes a weight rather than a call count.
      The ceiling is **read from `exchangeInfo`'s `rateLimits` array at connect time**, not
      hardcoded — a hardcoded number is one that will be wrong after Binance changes it,
      and we would find out from the ban.

- [ ] **Step 4: Mutation-test.** Remove the shared tier; make `Penalize` scope to one
      integration; drop the priority ordering. Each must fail a test.

- [ ] **Step 5: Commit.**

---

## Task 4: The signed REST client, and the fixtures everything else is built on

**Files:**
- Create: `backend/internal/exchange/binance/client.go`, `spot.go`
- Create: `backend/cmd/plimsollctl/record.go` — the fixture recorder
- Create: `backend/testdata/fixtures/binance/*.json`
- Test: `backend/internal/exchange/binance/client_test.go` (unit, `httptest`)

**Consumes:** task 1 credentials, task 3 limiter.
**Produces:**
- `binance.Client` — `NewClient(cred, limiter, baseURL string) *Client`
- `binance.ErrRateLimited`, `binance.ErrBanned`, `binance.ErrPermission`
- `(*Client).ExchangeInfo`, `.Account`, `.MyTrades`, `.DepositHistory`,
  `.WithdrawHistory`, `.APIRestrictions`

**No order endpoint is wrapped.** Not unused, not behind a flag, not in a comment (L13).

- [ ] **Step 1: Write the fixture recorder first.** `plimsollctl record -endpoint myTrades
      -symbol BTCUSDT` writes the raw response to `testdata/fixtures/binance/`, with
      redaction applied **in the write path** — never "record now, redact later", which is
      how a secret reaches git. Redact: the API key header, account ids, and any address
      field. Keep every numeric field exactly as sent, as a string.

- [ ] **Step 2: Verify F5 with the recorder.** Fetch `myTrades` with `fromId=0&limit=1000`
      and no time range. If it returns the account's oldest trades, F5 holds and task 6
      walks by id. If it errors or silently applies a window, **stop and revise task 6 to
      24-hour chunks** before writing it. Record the result either way — this is the one
      inference in `BINANCE-API-NOTES.md` and the plan is built on it.

- [ ] **Step 3: Write the failing client tests** against `httptest`:
      - the signature is `HMAC-SHA256` over the exact query string, and `X-MBX-APIKEY`
        carries the key — asserted against a known vector, not against our own output
      - `X-MBX-USED-WEIGHT-1M` is read from the response and reported to the limiter
      - a `429` with `Retry-After` returns `ErrRateLimited` carrying the duration and
        calls `Penalize`
      - a `418` returns `ErrBanned` and **does not retry** — retrying a ban extends it
      - a 5xx retries with backoff; a 4xx other than 429/418 does not
      - **no error, log line or panic message ever contains the secret** — drive a failure
        through every path and grep the output

- [ ] **Step 4: Run and watch them fail.**

- [ ] **Step 5: Implement.** `context.Context` first parameter throughout. Every call takes
      its weight from a table of constants that cites `BINANCE-API-NOTES.md` §2, so a
      reviewer can check a number against the source without leaving the file.

- [ ] **Step 6: Mutation-test.** Retry on 418; drop the `Penalize` call; sign the query in
      the wrong order. Each must fail a test.

- [ ] **Step 7: Commit.**

---

## Task 5: The normalizer — payload to canonical event

**Files:**
- Create: `backend/internal/exchange/binance/normalize.go`
- Test: `backend/internal/exchange/binance/normalize_test.go` (unit, golden)

**Consumes:** task 4 fixtures, `asset.Resolve` and `instrument.Resolve` from M1.
**Produces:**
- `binance.NormalizeSpotTrade(raw json.RawMessage, ...) (ledger.Event, error)`
- `binance.NormalizeDeposit`, `binance.NormalizeWithdrawal`
- `binance.NormalizeStreamExecutionReport(raw json.RawMessage, ...) (ledger.Event, error)`

This is the layer M1 deliberately deferred (K31), and it is where L5 is either satisfied or
quietly broken.

- [ ] **Step 1: Write the failing tests. The first one is the milestone's exit criterion:**
      - **the same trade, taken from the REST `myTrades` fixture and from the WS
        `executionReport` fixture, normalizes to the same `venue_event_id` and the same
        quantity, price, fee and `event_time`.** Different `source`, one identity. This is
        what makes M1's dedup test mean something against real data
      - every money field parses through `decimal.NewFromString`; a fixture value with 18
        decimal places survives intact
      - the symbol is resolved through `instrument.Resolve` **at the event's own
        `event_time`** (L8) — a test with two alias windows proves the resolver is called
        with the trade's time and not with `time.Now()`
      - an unresolvable symbol returns an error carrying the symbol, and the caller turns
        it into an `unknown_symbol` finding (K14) rather than dropping the trade
      - `isBuyer`/`isMaker` map to `side` correctly, both ways round
      - the commission and `commissionAsset` land on `fee`/`fee_asset` of that event and
        **never** in the price (L9)
      - `raw` is the payload verbatim — byte-compare against the fixture (L15)
      - an unknown or new field in the payload does **not** fail normalization, but an
        unknown enum value (an execution type we do not model) **does**

- [ ] **Step 2: Run and watch them fail.**

- [ ] **Step 3: Implement.** `venue_event_id` is `spot:trade:<symbol>:<tradeId>` for both
      paths; deposits use `spot:deposit:<txId>`, withdrawals `spot:withdrawal:<id>`.
      `venue_sequence` is the venue's own id. `event_time` is the exchange's timestamp,
      never ours (K2).

- [ ] **Step 4: Mutation-test.** Put `source` into the id; resolve with `time.Now()`; fold
      the commission into the price. Each must fail a test.

- [ ] **Step 5: Commit.**

---

## Task 6: Discovery and the resumable backfill

**Files:**
- Create: `backend/migrations/00009_backfill_progress.sql`
- Create: `backend/internal/backfill/discover.go`, `walk.go`
- Test: `backend/internal/backfill/backfill_integration_test.go`

**Consumes:** tasks 3–5.
**Produces:**
- `backfill.Discover(ctx, c *binance.Client, integrationID) ([]string, error)`
- `backfill.Walk(ctx, deps, accountID, integrationID, symbol string) error`
- `backfill.Progress{Scope string, Cursor string, CompletedAt *time.Time}`

**Why this task is shaped the way it is (F4).** There is no endpoint that returns spot
trades across symbols, and every cheap discovery has the same hole: an asset bought and
fully sold leaves no trace in current balances, in deposits, or in withdrawals. Its only
record is its trades, which cannot be found without already knowing the symbol. K26 refuses
holes — a missing acquisition makes the ledger produce a negative balance and poisons the
cost basis permanently.

So discovery probes **every** spot symbol once, `myTrades?symbol=X&limit=1`. Two to three
thousand symbols at weight 20 is 40–60k weight: under ten minutes of a dedicated budget,
once per integration, at backfill priority where it cannot starve anyone's live stream. It
is not clever, and it is the only version with no hole in it.

- [ ] **Step 1: Write the migration.** `backfill_progress (account_id, integration_id,
      scope, cursor, completed_at, updated_at)`, PK `(integration_id, scope)`. `scope` is
      `discover` or `trades:<symbol>` or `deposits` or `withdrawals`. Tenant table: RLS
      enabled and forced, `account_id` on the row (L12).

- [ ] **Step 2: Write the failing integration tests.**
      - **resume:** walk a symbol, kill it halfway (return a context error after N pages),
        walk again — every trade lands exactly once and none is skipped. This is the M2
        exit criterion "backfill resumes after interruption"
      - **idempotency across paths:** append the same trade through the REST walk and
        through the stream normalizer; the ledger holds one row (L5, and now with real
        payloads)
      - a symbol the account never traded is probed once and recorded as complete, so a
        rerun does not probe it again
      - discovery that is interrupted resumes from the symbol it stopped at, rather than
        starting the probe sweep over
      - progress is per scope: finishing `trades:BTCUSDT` does not mark `deposits` done
      - deposits and withdrawals walk in **90-day** windows (their documented limit),
        which is a different chunking rule from trades — the test asserts the window size
        rather than trusting the caller

- [ ] **Step 3: Run and watch them fail.**

- [ ] **Step 4: Implement.** Trades walk by `fromId` (F5), one weight-20 request per 1000
      trades, cursor = last trade id seen. Deposits and withdrawals walk by time window,
      cursor = window end. Every page is appended through `ledger.Append` inside
      `tenancy.InTx`, and the cursor advances **in the same transaction as the events it
      describes** — otherwise a crash between the two either loses events or replays them.

- [ ] **Step 5: Mutation-test.** Advance the cursor in its own transaction; make the cursor
      global rather than per scope; skip the probe-complete record. Each must fail a test.

- [ ] **Step 6: Commit.**

---

## Task 7: The live stream

**Files:**
- Create: `backend/internal/exchange/binance/stream.go`
- Test: `backend/internal/exchange/binance/stream_test.go` (unit, fake WS server)

**Produces:**
- `binance.Stream` — `Subscribe(ctx) (<-chan json.RawMessage, error)`, `Close() error`
- `binance.ErrGap`

**F1 governs this task.** Spot `listenKey` was removed after the 2025-04-07 announcement;
user data now arrives over the WebSocket API. Per the decision above we use
`userDataStream.subscribe.signature`, which works with the HMAC key we already store. There
is **no listenKey lifecycle here** — if this task grows one, it has copied a stale tutorial.

- [ ] **Step 1: Record a stream fixture.** Run against the live account long enough to
      capture at least one `executionReport` and one `outboundAccountPosition`, redact,
      commit. If the account is quiet, place and cancel a far-from-market limit order by
      hand in the Binance UI — **not through the API**, which our key cannot do and must
      not be able to.

- [ ] **Step 2: Write the failing tests** against a fake WS server:
      - a subscribe request is signed correctly and the connection carries the events
      - a dropped connection reconnects with backoff and re-subscribes
      - **a reconnect emits `ErrGap` for the window it was disconnected**, because events
        during that window were missed and nothing in the protocol will tell us what they
        were. The supervisor turns that into a bounded REST resync (task 8)
      - an unparseable frame is logged and skipped without killing the stream, but is
        counted — a stream that silently drops every frame looks identical to a quiet
        account, which is the failure L11 exists to prevent
      - `Close` during a reconnect backoff returns promptly

- [ ] **Step 3: Run, implement, mutation-test** (remove the gap signal; swallow the parse
      counter). **Commit.**

---

## Task 8: The lease, the supervisor, and telling the truth about state

**Files:**
- Create: `backend/migrations/00010_integration_leases.sql`
- Create: `backend/internal/worker/lease.go`, `supervisor.go`
- Modify: `backend/cmd/worker/main.go`, `backend/internal/httpapi/envelope.go`
- Test: `backend/internal/worker/lease_integration_test.go`,
  `supervisor_integration_test.go`

**Consumes:** every task above.
**Produces:**
- `worker.Claim(ctx, db, integrationID, ownerID) (bool, error)`, `.Heartbeat`, `.Release`
- `worker.Supervisor` with states `connecting · live · degraded · resyncing · backfilling`
- `httpapi.ReasonHistoryTruncated` — a new reason code (see below)

- [ ] **Step 1: Write the failing lease tests.**
      - two workers race to claim one integration; **exactly one wins**. Run it with
        `-race` and with real concurrent transactions, because the claim is a single
        `INSERT … ON CONFLICT … WHERE expires_at < now()` and its whole value is that it
        is atomic (K20)
      - an expired lease is claimable; a live one is not
      - a released lease is immediately claimable
      - a worker that stops heartbeating loses the lease after expiry

- [ ] **Step 2: Add the freshness reason.** `history_truncated` joins the closed set in
      `ARCHITECTURE.md` §5 and `httpapi/envelope.go`, severity `warn`.

      **Why it is not `backfill_incomplete`:** that reason means "not finished yet", which
      is recoverable and implies waiting will fix it. A venue that will never return data
      before a cutoff is a different and permanent claim, and telling a user to wait for
      something that will not arrive is exactly the confident-and-wrong failure L11 rejects.
      Spot's history depth is listed as unverified in `BINANCE-API-NOTES.md` §5; F2 makes
      it certain for futures at M5. Adding it here means M5 reports it instead of inventing
      a reason under pressure.

- [ ] **Step 3: Write the failing supervisor tests.**
      - each state maps to exactly one freshness reason, asserted by walking the state
        enum — so a state added later without a reason fails here rather than shipping
        silent. This is the same shape as M0's default-deny route test
      - a `ErrGap` from the stream moves `live → resyncing`, runs a **bounded** REST
        resync over the gap window, and returns to `live`
      - backfill chunks are interleaved with realtime and never block it (K24 priority)
      - losing the lease stops the supervisor before it writes anything further

- [ ] **Step 4: Run, implement, mutation-test** (let two workers claim; drop a state's
      reason mapping; make the resync unbounded). **Commit.**

- [ ] **Step 5: End-to-end against the real account.**
      Connect a real read-only key, run the full backfill, let the stream run, then:

      ```
      make test && make test-integration && make lint
      go run ./cmd/plimsollctl rebuild -integration <id>
      ```

      Assert by hand and record the output in the commit message:
      - the position for a symbol traded by hand matches the Binance UI
      - `Rebuild` reproduces the projection byte-identically (L3, now against real data)
      - re-running the backfill inserts **zero** rows (L5 across REST and WS, for real)

- [ ] **Step 6: Update the docs.** Mark M2 in `PROJECT.md` §8; record this plan's decisions
      in `DECISIONS.md` as K33+; re-verify `BINANCE-API-NOTES.md` and correct anything the
      live API contradicted — especially F5.

---

## Verification

M2 is complete when each of these has observed output:

| Exit criterion (PROJECT.md §8) | How it is proven |
|---|---|
| Real account history → ledger | Task 8 step 5, against a real key |
| Idempotency across REST and WS | Task 5's one-identity test, plus a rerun inserting zero rows |
| Backfill resumes after interruption | Task 6's kill-halfway test |
| Key is read-only | Task 2, including the rejection fixtures |
| No credential in a log | Task 1 step 7 and task 4 step 3, both mutation-tested |
| Rate limits respected | Task 3's shared-tier test; no 418 during the real backfill |

**Then hand to Codex.** The highest-value review targets, in order:

1. **`normalize.go`** — if REST and WS ever produce different ids for one trade, every
   position doubles and no test outside task 5 will notice.
2. **The backfill cursor** — that it advances in the same transaction as the events it
   describes, and that no path advances it on a partial page.
3. **`crypto/envelope.go`** — nonce reuse, and whether any error path can leak key
   material.
4. **The lease claim statement** — that it is genuinely atomic under concurrent workers.

**Not in M2, by decision:** USD-M futures ingestion, funding and `positionRisk` (M5);
valuation and prices (M4); the portfolio API and lineage (M3); Ed25519 credentials (only
if Binance removes the HMAC path).
