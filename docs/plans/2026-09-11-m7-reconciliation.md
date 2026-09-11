# M7 — Reconciliation and Data Quality Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the product's actual claim testable. Everything before M7 computes numbers
from our own ledger; M7 is the milestone that asks the exchange whether we are right, and
publishes the answer whether or not it flatters us.

**Architecture:** Two modules answering two different questions, one register, one endpoint.
`quality` asks *is our state internally coherent?* and needs no exchange call.
`reconciliation` asks *does our state match the exchange's?* and pulls a REST snapshot to
find out. Both emit `quality.Finding` values into one register with an **open/close
lifetime**, so a mismatch that persists for a day is one finding, not two hundred and
eighty-eight. Both are pure functions of their inputs (L4); the I/O lives in the worker and
the store.

**Tech Stack:** Go 1.26 · pgx v5 · sqlc · goose · shopspring/decimal · Huma v2 · Next.js

**Spec:** `docs/PROJECT.md` §8 (M7 row) · `docs/ARCHITECTURE.md` §8 (reconciliation and data
quality), §5 (freshness) · `docs/DECISIONS.md` K14 K23 K26 K33 · `docs/BINANCE-API-NOTES.md`
§3 (weights) · `CLAUDE.md` L2 L3 L4 L11 L12 L15

---

## Context

`freshness.ReasonReconciliationMismatch` has existed since M0 and **nothing has ever
produced it.** The constant is declared, two tests name it, and no code path can raise it.
That is the K38 defect for the third time: a guard that is written, tested, and never
called. M7 is the milestone that gives it a producer.

The checks in K14 are likewise half-built. `portfolio/freshness.go` computes
`negative_balance`, `unknown_symbol` and `fee_price_missing` **per response** — so a user
who never loads `/portfolio` is never told. A standing check is a different thing from a
response annotation: it runs whether or not anyone is looking, and it remembers.

Three decisions were taken before writing this plan, and they are recorded as K53–K55.

### K53 — A finding has a lifetime, not a timestamp

Reconciliation runs every five minutes. A balance that is wrong all day is **one** problem.
If each run inserted a row, the register would grow by 288 rows a day per subject, and the
one question the user actually asks — *is it still wrong?* — would become a query over
history.

So a finding has identity `(account_id, integration_id, kind, subject)` and a lifetime: a run
**opens** what it newly sees, **touches** what it still sees, and **closes** what it no longer
sees. This is deliberately the same shape as M6's alert hysteresis, because it is the same
failure: a repeating condition must not become a repeating record.

`last_seen_at` and `occurrences` are updated on an open finding. That is an UPDATE, and it is
allowed, because the register is a projection — not the ledger (L2, L3). It is rebuildable:
drop it and the next run repopulates it from the ledger and the exchange.

### K54 — Tolerance is per metric, and the classifier uses evidence rather than sign

One epsilon cannot serve both a quantity in BTC and a value in USD: pick a number small
enough for USD and every dust balance is a finding; pick one large enough for BTC dust and a
$400 discrepancy is invisible. So tolerance is per metric — quantity against a per-asset dust
threshold, value against a basis-point band.

The classifier is the harder half. The obvious mapping — *they have more than us, so we are
missing an event; we have more than them, so we counted one twice* — **is wrong**, because
"we have more than them" is equally explained by a withdrawal we never ingested. Sign alone
decides nothing.

So three classes are decided from evidence, and the fourth is the honest residual:

| Class | Decided by |
|---|---|
| `rounding` | the delta is smaller than one step of the subject's own precision — a representation difference, not a missing fact |
| `unsupported` | our ledger holds a record for this subject that we deliberately do not normalize (a withdrawal, an unresolved asset), large enough to explain the delta |
| `duplicate` | the ledger holds two events with the same venue identity under different integrations — the one way L5's dedup key can still let a trade in twice |
| `missing_event` | **residual.** Outside tolerance, and none of the above explains it. |

`missing_event` as the residual is correct rather than lazy: it is the class that means *we
cannot account for this*, which is precisely what the user needs to be told.

### K55 — Resync re-runs the walk; it never writes a correction

V1 policy is detect and report, never auto-correct (`ARCHITECTURE.md` §8). "A resync action"
must not be read as automatic repair. Resync **re-opens a backfill scope's cursor** so the
history walk runs again; anything genuinely missing is appended by the ordinary ingest path
under the ordinary dedup key, and anything already present is deduplicated away (L5).

It writes no correction event and touches no `ledger_events` row (L2). Auto-correcting a
*misclassified* finding writes a wrong correction into an append-only ledger, and that cannot
be undone — which is exactly why classification must be validated against a real account
before V2 is allowed to act on it.

---

## Global Constraints

Every task's requirements implicitly include this section.

- **L1** — money is `NUMERIC(38,18)` / `decimal.Decimal` / JSON string. A tolerance is money
  too: no `float64` epsilon anywhere in this milestone.
- **L2** — the ledger is append-only. Nothing in M7 updates or deletes a ledger row. The
  findings register is a projection and may be updated (K53).
- **L3** — the register survives being dropped: the next run rebuilds it.
- **L4** — `quality.Check` and `reconciliation.Compare` are pure. The snapshot, the
  projection and `now` are inputs.
- **L11** — an open finding above tolerance raises `reconciliation_mismatch` on the responses
  it affects. Degraded and visible beats confident and wrong.
- **L12** — `findings` carries `account_id`, RLS enabled **and** forced, and the composite FK
  `(account_id, integration_id)`.
- **L15** — the exchange snapshot that produced a finding is stored raw beside it. When a
  classification turns out wrong months later, the payload is the only thing that can settle it.
- Reconciliation runs at priority `reconciliation` — below realtime, above backfill
  (`ARCHITECTURE.md` §7). `GET /api/v3/account` costs weight 20, `positionRisk` weight 5.
- **Never** request a permission beyond read-only; never implement execution (L13).

---

## File Structure

```
backend/migrations/
  00027_findings.sql                  the register, its lifetime columns, RLS
backend/internal/quality/
  finding.go                          Kind, Severity, Subject, Finding — the vocabulary
  check.go                            the internal-coherence checks (K14) — pure
  store.go                            open / touch / close, and the read for the endpoint
backend/internal/reconciliation/
  snapshot.go                         Ours / Theirs: the two sides, as plain data
  compare.go                          Compare(ours, theirs, Tolerance) []Delta — pure
  classify.go                         the evidence-based classifier (K54) — pure
  runner.go                           the I/O: pull, compare, record (worker-side)
backend/internal/exchange/binance/
  account.go                          DecodeSpotAccount — balances + updateTime (F21)
backend/internal/httpapi/
  quality.go                          GET /data-quality · POST /integrations/{id}/resync
frontend/app/quality/page.tsx         the register, as a page
```

---

## Task 1: The register — schema, vocabulary, lifetime

**Files:**
- Create: `backend/migrations/00027_findings.sql`
- Create: `backend/internal/quality/finding.go`, `backend/internal/quality/store.go`
- Create: `backend/internal/store/queries/findings.sql`
- Test: `backend/internal/quality/store_integration_test.go`,
  `backend/internal/quality/schema_integration_test.go`

**Interfaces:**
- Produces: `quality.Kind` (`missing_event` `duplicate` `rounding` `unsupported`
  `negative_balance` `unresolved_asset` `fee_price_missing` `clock_skew` `snapshot_failed`);
  `quality.Finding{Kind, Subject, Severity, Detail, Delta decimal.NullDecimal, Raw json.RawMessage}`;
  `quality.Record(ctx, tx, accountID, integrationID, runAt, found []Finding) error` — the
  whole lifetime in one call; `quality.Open(ctx, tx, accountID) ([]Stored, error)`.

- [ ] **Step 1: Write the failing lifetime tests**

The test that matters is not "a finding can be written". It is that the same finding seen
twice is still one finding, and one that stops being seen closes itself.

```go
// Two runs seeing the same problem produce one finding, not two. A mismatch that persists
// all day is one problem the user has, and the register must say so (K53).
func TestTheSameFindingSeenTwiceStaysOneFinding(t *testing.T)

// A finding the next run does not see is closed, with the instant it stopped being true.
// Without this the register only grows, and "is it still wrong?" becomes unanswerable.
func TestAFindingThatStopsBeingTrueIsClosed(t *testing.T)

// Closing is not deletion: the history of a finding is evidence, and evidence is kept.
func TestAClosedFindingIsStillReadable(t *testing.T)

// A finding that returns after closing is a NEW finding, not a resurrection. "Wrong for an
// hour, right for a day, wrong again" is two incidents; reporting it as one loses the
// recovery in between.
func TestAFindingThatReturnsAfterClosingIsANewFinding(t *testing.T)
```

- [ ] **Step 2: Run to verify failure** — `package .../internal/quality is not in std`.

- [ ] **Step 3: Write the migration**

`findings` carries `account_id`, `integration_id`, `kind`, `subject`, `severity`, `detail`,
`delta NUMERIC(38,18)`, `raw JSONB`, `opened_at`, `last_seen_at`, `closed_at`, `occurrences`.

- A partial unique index enforces identity **while open**:
  `CREATE UNIQUE INDEX findings_one_open_per_subject ON findings (account_id, integration_id, kind, subject) WHERE closed_at IS NULL;`
  Closed rows fall outside the index, which is exactly what makes "a return is a new finding"
  work without a second table.
- `CHECK (closed_at IS NULL OR closed_at >= opened_at)`, `CHECK (occurrences >= 1)`.
- `account_id`, RLS enabled and forced, composite FK `(account_id, integration_id)` (L12).
- `GRANT SELECT, INSERT, UPDATE ON findings TO plimsoll_app;` — **no DELETE**. A finding
  closes; it does not vanish.

- [ ] **Step 4: Implement `finding.go` and `store.go`**

`Record` performs the whole lifetime in one transaction: upsert each found finding onto the
partial unique index (bumping `occurrences` and `last_seen_at`), then close every open
finding of the same `(integration_id, kind)` family whose subject was absent from this run.
One transaction is what makes a crash mid-run leave the register coherent rather than half-swept.

- [ ] **Step 5: Run the tests** — expected PASS.

- [ ] **Step 6: Mutation-test the guards**

Drop `WHERE closed_at IS NULL` from the partial index and confirm
`TestAFindingThatReturnsAfterClosingIsANewFinding` fails **by name**; then disable the close
sweep and confirm `TestAFindingThatStopsBeingTrueIsClosed` fails. Report any survivor honestly.

- [ ] **Step 7: Commit** — `feat(quality): a findings register where a problem has a lifetime (K53)`

---

## Task 2: The internal-coherence checks — pure, no exchange call

**Files:**
- Create: `backend/internal/quality/check.go`
- Test: `backend/internal/quality/check_test.go` (unit — no Docker)

**Interfaces:**
- Consumes: `quality.Finding`.
- Produces: `quality.Input{Balances, Positions, Unresolved, LastIngestAt, ExchangeClock, Now, SkewTolerance}`
  and `quality.Check(in Input) []Finding` — pure (L4).

- [ ] **Step 1: Write the failing tests**

```go
// The strongest signal in the system: the ledger implies selling more than was ever held,
// so an event is missing — and the check finds it WITHOUT knowing which one. That property
// is the whole point, so the test asserts it from a balance alone.
func TestANegativeBalanceIsAMissingEventWithoutKnowingWhichOne(t *testing.T)

// Exactly zero is not negative. The boundary is the entire check.
func TestAZeroBalanceIsNotAFinding(t *testing.T)

// Dust below the asset's own precision is a rounding artefact, not a missing event —
// otherwise an account with a 1e-18 remainder is permanently "unreliable" and the register
// becomes noise the user learns to skip.
func TestDustBelowPrecisionIsRoundingNotMissingEvent(t *testing.T)

// An asset we cannot resolve is reported as unresolved rather than valued at zero. Valuing
// it at zero makes the total quietly wrong instead of loudly incomplete (L11).
func TestAnUnresolvableAssetIsAFindingNotAZero(t *testing.T)

// The exchange's clock against ours: beyond tolerance every timestamp-ordered fold is
// suspect, so it is reported before it can corrupt an ordering (L7).
func TestClockSkewBeyondToleranceIsReported(t *testing.T)

// Skew is symmetric: being ahead of the exchange is exactly as bad as being behind it.
func TestClockSkewIsReportedInBothDirections(t *testing.T)

// L4: the same input twice gives the same findings, and Check never reads a clock.
func TestCheckIsPure(t *testing.T)
```

- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement `check.go`** — no `time.Now()`, no DB handle, no logger.
- [ ] **Step 4: Run the tests** — expected PASS.
- [ ] **Step 5: Mutation-test the boundaries.** Flip `<` to `<=` on the zero-balance
  comparison and on the skew tolerance; confirm a *named* test fails for each.
- [ ] **Step 6: Commit** — `feat(quality): the checks that need no exchange call (K14)`

---

## Task 3: The comparison and the classifier — pure

**Files:**
- Create: `backend/internal/reconciliation/snapshot.go`, `compare.go`, `classify.go`
- Create: `backend/internal/exchange/binance/account.go`
- Test: `backend/internal/reconciliation/compare_test.go`, `classify_test.go`,
  `backend/internal/exchange/binance/account_test.go`
- Create fixture: `backend/testdata/fixtures/binance/spot_account_documented_example.json`

**Interfaces:**
- Consumes: `collateral.Snapshot` (M5 already pulls `positionRisk`), `quality.Finding`.
- Produces:
  - `binance.DecodeSpotAccount(raw json.RawMessage) (SpotAccount, error)` where
    `SpotAccount{UpdateTime time.Time, Balances []SpotBalance}` and
    `SpotBalance{Asset string, Free, Locked decimal.Decimal}`
  - `reconciliation.Ours{Balances map[string]decimal.Decimal, Positions map[int64]decimal.Decimal}`
    and `reconciliation.Theirs{...}`
  - `reconciliation.Tolerance{Dust map[string]decimal.Decimal, DefaultDust decimal.Decimal, ValueBasisPoints decimal.Decimal}`
  - `reconciliation.Compare(ours Ours, theirs Theirs, tol Tolerance) []Delta`
  - `reconciliation.Classify(d Delta, ev Evidence) quality.Kind`

**Verified against the official docs, not memory (F21):** `GET /api/v3/account` costs weight
20 and returns `balances: [{asset, free, locked}]` beside a millisecond `updateTime`. That
`updateTime` is what lets the snapshot carry the **exchange's** instant rather than ours —
and the difference between the two is Task 2's clock-skew check, for free.

- [ ] **Step 1: Write the failing tests**

```go
// free + locked is the holding. Comparing against `free` alone reports every open order as a
// missing event, which is the single easiest way to make this feature useless.
func TestLockedBalanceCountsAsHeld(t *testing.T)

// An asset they report and we have never heard of is a finding, not an absence. Iterating
// only our own keys is how an entirely missing asset stays invisible.
func TestAnAssetOnlyTheExchangeKnowsIsAFinding(t *testing.T)

// ...and symmetrically, one only we believe in.
func TestAnAssetOnlyWeKnowIsAFinding(t *testing.T)

// Agreement within tolerance produces nothing at all: the register must stay quiet when we
// are right, or nobody will read it when we are not.
func TestAgreementWithinToleranceProducesNothing(t *testing.T)

// Tolerance is per asset: a delta that is dust in SHIB is a real position in BTC. One epsilon
// for both is either constant noise or silent blindness (K54).
func TestToleranceIsPerAssetNotGlobal(t *testing.T)

// THE CLASSIFIER TEST THAT MATTERS. We hold more than they do, and the ledger contains an
// un-normalized withdrawal large enough to explain it: that is `unsupported`, NOT
// `duplicate`. Calling it a duplicate sends the user hunting for a double-count that does
// not exist (K54).
func TestHoldingMoreThanTheExchangeIsNotAutomaticallyADuplicate(t *testing.T)

// The same delta with no evidence at all is the residual: we cannot account for it.
func TestAnUnexplainedDeltaIsAMissingEvent(t *testing.T)

// A true duplicate is decided from the ledger: one venue identity, two integrations (L5).
func TestOneVenueIdentityUnderTwoIntegrationsIsADuplicate(t *testing.T)

// L4.
func TestCompareIsPure(t *testing.T)
```

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Record the fixture**

No real key exists, so the spot account payload has never been recorded. Write the fixture
from the **documented example**, and name it `documented_example` rather than `recorded` —
that distinction already exists in `testdata/fixtures/binance/README.md` and it must not
blur. A key that arrives later replaces it with a recorded one via `plimsollctl record`.

- [ ] **Step 4: Implement decode, compare and classify**

Field lookup is an **exact map lookup**, never struct tags: `encoding/json` matches field
names case-insensitively, which is how `json:"s"` was once filled by `"S"` (F20).

- [ ] **Step 5: Run the tests** — expected PASS.
- [ ] **Step 6: Mutation test.** Drop `locked` from the sum and confirm
  `TestLockedBalanceCountsAsHeld` fails; make the classifier fall back to sign and confirm
  `TestHoldingMoreThanTheExchangeIsNotAutomaticallyADuplicate` fails.
- [ ] **Step 7: Commit** —
  `feat(reconciliation): compare against the exchange, classify from evidence (K54)`

---

## Task 4: The runner, the resync, and the endpoint

**Files:**
- Create: `backend/internal/reconciliation/runner.go`, `backend/internal/httpapi/quality.go`
- Modify: `backend/internal/worker/supervisor.go` (a `Reconciler` beside the `Capturer`),
  `backend/cmd/worker/main.go`, `backend/internal/portfolio/freshness.go`,
  `backend/internal/portfolio/risk.go`
- Create: `frontend/app/quality/page.tsx`
- Test: `backend/internal/reconciliation/runner_integration_test.go`,
  `backend/internal/httpapi/quality_integration_test.go`

**Interfaces:**
- Produces: `GET /data-quality`, `POST /integrations/{id}/resync`,
  `reconciliation.Run(ctx, deps, integrationID, now) error`.

- [ ] **Step 1: Write the failing tests**

```go
// The milestone in one test: a ledger and an exchange that disagree produce an OPEN finding,
// and the portfolio reading that ledger is served with reconciliation_mismatch rather than
// served silently (L11). This is the test that finally gives the constant a producer.
func TestADisagreementReachesTheResponseAsAFreshnessReason(t *testing.T)

// A run that agrees closes what a previous run opened, and the reason disappears with it.
func TestAgreementClearsTheReasonAndClosesTheFinding(t *testing.T)

// The snapshot that produced the finding is stored beside it (L15). Without the payload, a
// classification that turns out wrong in three months cannot be re-argued.
func TestTheSnapshotIsStoredWithTheFinding(t *testing.T)

// Two workers, one integration: the lease means one reconciliation run, not two.
func TestReconciliationRunsUnderTheLease(t *testing.T)

// K55: resync re-opens the walk. It appends nothing itself, writes no correction, and leaves
// every existing ledger row byte-identical (L2).
func TestResyncReopensTheScopeAndWritesNoCorrection(t *testing.T)

// Resync is scoped to the caller's own integration. Another account's id must 404, not 403:
// confirming that a resource exists is itself a leak (L12).
func TestResyncRefusesAnotherAccountsIntegration(t *testing.T)

// An exchange call that fails is itself a finding. "We could not check" is not the same claim
// as "we checked and it was fine", and serving the second while the first is true is exactly
// the failure L11 exists to prevent.
func TestAFailedSnapshotIsReportedRatherThanSkipped(t *testing.T)
```

- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement the runner** — lease-guarded, priority `reconciliation`, every five
  minutes, one transaction per run so `Record`'s lifetime sweep stays atomic.
- [ ] **Step 4: Implement `GET /data-quality` and `POST /integrations/{id}/resync`.**
- [ ] **Step 5: Wire the freshness reason** into `/portfolio`, `/risk` and `/exposure`.
- [ ] **Step 6: Implement the dashboard page** — it parses no number (K52); open and closed
  findings are visually distinct, and a delta renders as the string it arrived as.
- [ ] **Step 7: Run every gate** — `make lint`, `make test`, `make test-integration`,
  `make frontend-check`, `make generate` (no diff), `make docs-check`, `bash deploy/smoke.sh`.
  Report the actual output of each; never round a partial result up to done.
- [ ] **Step 8: Commit and open the PR.**

---

## Verification

M7 is complete when each of these is true, with observed output:

| Exit criterion (PROJECT.md §8) | How it is proven |
|---|---|
| Classified findings | `classify_test.go` — all four classes, including the sign-is-not-evidence case |
| `missing_event` | `TestAnUnexplainedDeltaIsAMissingEvent` |
| `duplicate` | `TestOneVenueIdentityUnderTwoIntegrationsIsADuplicate` |
| `rounding` | `TestDustBelowPrecisionIsRoundingNotMissingEvent` |
| `unsupported` | `TestHoldingMoreThanTheExchangeIsNotAutomaticallyADuplicate` |
| A resync action | `TestResyncReopensTheScopeAndWritesNoCorrection` |
| `reconciliation_mismatch` finally has a producer | `TestADisagreementReachesTheResponseAsAFreshnessReason` |
| The register does not grow with time (K53) | `TestTheSameFindingSeenTwiceStaysOneFinding` |
| The raw payload is kept (L15) | `TestTheSnapshotIsStoredWithTheFinding` |
| Tenancy (L12) | `TestResyncRefusesAnotherAccountsIntegration` + the schema test |

**Not in M7, by decision:** auto-correction (V2, and only once classification has been
validated against a real account — K55); `sapi/v1/accountSnapshot`-based historical
reconciliation (IP weight 2400, `BINANCE-API-NOTES.md` §3); Bybit (M8).

**What M7 cannot prove without a key.** The comparison runs against a fake exchange in the
integration suite, exactly as M2's backfill does. Whether a real Binance account's balances
agree with our fold on the first try is precisely the thing that needs the key M2 is waiting
on — and it is the reason M7 detects rather than corrects (K55).
