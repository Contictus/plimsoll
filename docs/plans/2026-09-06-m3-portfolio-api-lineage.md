# M3 — Portfolio, API and Lineage · Implementation Plan

**Goal:** Turn the ledger M2 fills into numbers a reader can see and challenge:
`GET /portfolio`, `GET /positions`, `GET /positions/{id}`,
`GET /positions/{id}/lineage`, `GET /transactions`.

**Spec:** `docs/PROJECT.md` §5 §8 (M3 row), `docs/ARCHITECTURE.md` §3 §5 §10,
`docs/DECISIONS.md` K11 K20 K23, `CLAUDE.md` L1 L3 L6 L10 L11 L12

---

## Context

M2 ends with a worker that appends canonical events. It ends with something else too,
found while planning this milestone: **nothing calls `projection.Project`.** The fold is
written, tested and rebuild-equal, and no process runs it. `positions` is empty on a
live system. An endpoint reading it would have returned an empty portfolio for an
account with a full ledger — confident, and wrong, which is the one outcome L11 forbids.
Task 1 exists for that reason and comes first.

The second constraint shapes everything after it: **M4 owns prices.** There is no
`price_ticks`, no `valuation_run`, no USD. So M3 reports what the fold produces without
a price — quantity, average entry, cost basis, realized PnL, fees — and says so.

## The five decisions taken before writing this plan

| Question | Decision |
|---|---|
| Who runs the fold | The worker, under the same lease, on a ticker (K38) |
| How the API learns what the worker is doing | The worker publishes state to a table (K39) |
| What a portfolio total means with no prices | No total. Subtotals per quote asset, and `valuation_unavailable` (K40) |
| How the ledger is paginated for a reader | Canonical order, and the page is not final while a backfill runs (K41) |
| What a position's API id is | `<integration_id>.<instrument_id>` — natural, because a rebuild regenerates surrogates (K42) |

---

## Global Constraints

- **L1** — money is `NUMERIC(38,18)` / `decimal.Decimal` / JSON **string**. Every number
  in every response in this milestone is a string.
- **L3** — `positions` stays a projection. No endpoint writes it, and nothing in this
  milestone may make it a second source of truth.
- **L4** — `internal/portfolio` is pure. Time and rows are inputs.
- **L6** — no cursor advances on the global `seq`.
- **L10 / L11** — `as_of` and `freshness` on every response, and every degradation named.
- **L12** — every read goes through `tenancy.InTx`.

---

## Task 1: Run the fold, and publish what the worker is doing

**Files:**
- Create: `backend/migrations/00016_integration_status.sql`
- Create: `backend/internal/store/queries/status.sql`
- Create: `backend/internal/worker/status.go`
- Modify: `backend/internal/projection/project.go` (a lease guard hook)
- Modify: `backend/internal/worker/supervisor.go`, `adapters.go`, `backend/cmd/worker/supervise.go`
- Test: `backend/internal/worker/supervisor_integration_test.go`,
  `backend/internal/projection/project_integration_test.go`

**Produces:**
- `projection.WithGuard(g projection.Guard) projection.Option`
- `worker.LedgerProjector`, `worker.PublishStatus`, `worker.ReadStatus`
- `SupervisorConfig.Project`, `SupervisorConfig.ProjectEvery`

## Task 2: The portfolio engine

**Files:**
- Create: `backend/internal/portfolio/{portfolio.go,load.go,freshness.go}`
- Test: `backend/internal/portfolio/portfolio_test.go` (pure, no Docker),
  `backend/internal/portfolio/load_integration_test.go`

**Produces:** `portfolio.Build(in Input) Portfolio`, `portfolio.Load(ctx, db, accountID)`

## Task 3: `GET /portfolio`, `GET /positions`, `GET /positions/{id}`

**Files:**
- Create: `backend/internal/httpapi/portfolio.go`
- Modify: `backend/internal/httpapi/router.go`
- Test: `backend/internal/httpapi/portfolio_integration_test.go`

## Task 4: `GET /positions/{id}/lineage` and `GET /transactions`

**Files:**
- Create: `backend/internal/portfolio/lineage.go`, `backend/internal/httpapi/lineage.go`
- Modify: `backend/internal/store/queries/ledger.sql`
- Test: `backend/internal/portfolio/lineage_test.go`,
  `backend/internal/httpapi/lineage_integration_test.go`

---

## Verification

| Exit criterion (PROJECT.md §8) | How it is proven |
|---|---|
| `GET /portfolio` correct | Fixture ledger → worker folds → response matches the engine's own fold |
| `GET /positions/{id}/lineage` opens a position down to its events | Every event listed with the state it produced, replayed through `position.Apply` |
| The fold actually runs | A supervisor integration test asserts `positions` is populated without anyone calling `Project` |
| Numbers are strings (L1) | A guard test scans the rendered JSON for a bare number on a money field |
| Every response carries `as_of` + `freshness` (L10, L11) | The envelope is embedded, and a test walks every registered operation |
