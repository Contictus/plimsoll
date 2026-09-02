# M1 — Registry, Ledger, Position Engine · Implementation Plan

**Goal:** Turn canonical events into correct positions, offline. No network code and no
exchange client: M1 exists so the engine is right before live data can hide a bug in it.

**Architecture:** Identity first (`asset`, `instrument`, resolved through time-scoped
aliases), then the append-only `ledger_events` table, then a pure fold
(`position.Apply`) that a projector drives into the `positions` table. The fold has no
database handle and no clock (L4), which is what makes the invariant tests cheap and
scenario shocks (M7.5) nearly free later.

**Spec:** `docs/PROJECT.md` §2 §8 §9 · `docs/ARCHITECTURE.md` §3 §4 §12 ·
`docs/DECISIONS.md` K5 K10 K13 K18 K19 K20 K21 K22 · `CLAUDE.md` L1-L9

**Exit criteria (PROJECT.md §8):** fixture replay gives correct spot average cost and
realized PnL; time-scoped alias resolution tested; idempotency, order-independence and
rebuild-equality green.

---

## Global Constraints

Every task inherits `CLAUDE.md` §1 in full. The ones this milestone will actually be
tempted to break:

- **L1** money is `NUMERIC(38,18)` / `decimal.Decimal` / JSON string. The position engine
  is nothing but money arithmetic; one `float64` here poisons every number downstream.
- **L2/L3** the ledger is append-only and `positions` is a pure fold over it. Rebuild
  equality is mandatory and never skipped.
- **L4** `ledger` and `position` are pure. `make test` must still run without Docker.
- **L5** identity is `(integration_id, venue_event_id)`; `source` is never part of it.
- **L7** canonical order is `(event_time, venue_sequence, venue_event_id)`, never
  `event_time` alone.
- **L8** an exchange symbol is resolved through the alias table at the event's
  `event_time`, never with today's mapping.
- **L9** a fee rides on its parent event and is never folded into `avg_entry_price`.

---

## Decisions taken before writing this plan

Three, each because the documents as written cannot all be true at once.

### D1 - `integrations` lands in M1, without credentials

`ledger_events` carries `FOREIGN KEY (account_id, integration_id) REFERENCES
integrations (account_id, id)`. The M0 plan deferred `integrations` to M2, but a
composite FK cannot be added to a populated ledger without the table rebuild that M0
existed to avoid. So M1 creates `integrations` with identity and tenancy columns only;
M2 adds `credential_ciphertext`, `wrapped_dek` and `key_version` (K25).

### D2 - the strategy tag lives outside the `positions` projection

`PROJECT.md` §2 lists `strategy_id` as a column of `positions`, whose primary key is
`(integration_id, instrument_id)`. K13 settles the key question -- "strategy is assigned
at the position level, one strategy per position" -- so the key is right. But the tag is
**user-assigned input**, not folded state, and `positions` is droppable and rebuilt from
events (L3). A rebuild would therefore erase every strategy assignment the user made, and
the rebuild-equality test would pass while doing it, because both sides would be equally
empty.

Resolution: the assignment lives in its own table, keyed the same way, joined at read
time.

    position_strategies (account_id, integration_id, instrument_id, strategy_id, assigned_at)

`ledger_events.strategy_id` stays in the schema -- it is what a V2 lot-level split will
need under K5's lot-derivability constraint -- and is left NULL throughout M1.

### D3 - M1's fixtures are canonical events, not raw exchange payloads

The exit criterion says "fixture replay", and `CLAUDE.md` §2 says fixtures come before
network. But normalizing a raw Binance payload is the `exchange/binance` module, which is
M2. M1's golden fixtures are therefore canonical ledger events: hand-built, checked in,
and shaped exactly like what the M2 normalizer will have to produce. Raw payload fixtures
arrive with the normalizer that consumes them.

---

## File Structure

    backend/migrations/
      00004_assets.sql                  assets + asset_aliases, time-scoped
      00005_instruments.sql             instruments + instrument_aliases, market in key
      00006_integrations_ledger.sql     integrations (no credentials) + ledger_events
      00007_positions.sql               positions projection + position_strategies
    backend/internal/
      asset/resolver.go                 alias resolution at a point in time (K22, L8)
      instrument/resolver.go            same, with market as part of the key (K10)
      ledger/event.go                   the canonical event struct: pure, no pgx types
      ledger/append.go                  ON CONFLICT DO NOTHING as a correctness mechanism
      ledger/stream.go                  canonical-order read (L7)
      position/state.go                 State, the fold's accumulator
      position/apply.go                 Apply(State, Event) (State, error), pure (L4)
      position/project.go               the projector: stream, fold, write
    backend/testdata/golden/
      spot_basic.json                   buy, buy, sell: average cost and realized PnL
      spot_flip.json                    long to short in one trade (K5)
      fee_in_bnb.json                   fee in a third asset, never in avg cost (L9)

---

## Task 1: Asset registry and time-scoped alias resolution

**Files:** `migrations/00004_assets.sql`, `internal/asset/resolver.go`,
`internal/store/queries/asset.sql`
**Test:** `internal/asset/resolver_integration_test.go`

**Produces:**
- `asset.ErrUnknownSymbol`
- `asset.Resolve(ctx, q *store.Queries, source, externalSymbol string, at time.Time) (int64, error)`

- [ ] Write the failing tests: an overlapping alias window is rejected by the database;
      the same external symbol resolves to different assets before and after a recycle
      boundary; a symbol with no window covering `at` returns `ErrUnknownSymbol` rather
      than the nearest match.
- [ ] Run them and watch them fail because `assets` does not exist.
- [ ] Migration: `assets`, `asset_aliases` with `validity TSTZRANGE` and
      `EXCLUDE USING gist (source WITH =, external_symbol WITH =, validity WITH &&)`,
      which needs `btree_gist`. These are reference data shared across accounts, not
      tenant data, so they carry no `account_id` and no RLS -- with that reasoning written
      into the migration the way 00003 does it.
- [ ] Implement the resolver. It never guesses: an unresolved symbol is an error the
      caller must handle, which is what becomes an `unknown_symbol` finding in M3.5 (K14).
- [ ] `make generate`, tests green, `make lint` clean, commit.

---

## Task 2: Instrument registry, with market in the key

**Files:** `migrations/00005_instruments.sql`, `internal/instrument/resolver.go`
**Test:** `internal/instrument/resolver_integration_test.go`

**Consumes:** `asset.Resolve` -- an instrument's legs are assets.
**Produces:**
- `instrument.Market` (`spot` | `usdm`), `instrument.ErrUnknownSymbol`
- `instrument.Resolve(ctx, q, exchange string, market Market, exchangeSymbol string, at time.Time) (int64, error)`

- [ ] Write the failing test that is the whole point of this task: Binance `BTCUSDT`
      resolves to two different instruments depending on market, so the spot and perp
      positions cannot merge (K10). Plus the time-scoping tests from Task 1 and an
      exclusion constraint covering `(exchange, exchange_symbol, market)`.
- [ ] Run and watch it fail.
- [ ] Migration and resolver. `Market` is a typed parameter, not a string, so a caller
      cannot omit it or slide it into the wrong argument position.
- [ ] Tests green, lint clean, commit.

---

## Task 3: `integrations` and the append-only ledger

**Files:** `migrations/00006_integrations_ledger.sql`, `internal/ledger/event.go`,
`internal/ledger/append.go`, `internal/ledger/stream.go`
**Test:** `internal/ledger/ledger_integration_test.go`

**Produces:**
- `ledger.Event` -- the canonical event; `decimal.Decimal` money fields (L1), no pgx types
- `ledger.Append(ctx, q, events []Event) (inserted int, err error)`
- `ledger.Stream(ctx, q, integrationID uuid.UUID, after Cursor) ([]Event, error)`

- [ ] Write the failing tests, in this order because each guards a named law:
      - the same `venue_event_id` arriving twice under different `source` values inserts
        once (L5, K19) -- the test K3's original key would have failed
      - three events sharing one `event_time` come back ordered by `venue_sequence`, with
        a further tie broken by `venue_event_id` (L7, K21)
      - `raw` is `NOT NULL`, so an event cannot be stored without its payload (L15)
      - `UPDATE` and `DELETE` on `ledger_events` are refused for the app role (L2)
      - the composite FK rejects an event whose `integration_id` belongs to another
        account (K15)
- [ ] Run and watch them fail.
- [ ] Migration: `integrations` (D1) and `ledger_events` per `PROJECT.md` §2, with the
      unique constraint, the composite FK and the
      `(integration_id, event_time, venue_sequence)` index. Grant the app role
      `SELECT, INSERT` and nothing else, so L2 is a privilege rather than a convention.
- [ ] Implement append and stream. `Append` uses `ON CONFLICT DO NOTHING` and reports how
      many rows it actually inserted, because "we saw 40 events and stored 12" is the
      signal that makes a duplicate backfill visible instead of silent.
- [ ] Tests green, lint clean, commit.

---

## Task 4: The position engine, pure and without I/O

**Files:** `internal/position/state.go`, `internal/position/apply.go`
**Test:** `internal/position/apply_test.go` (unit, no Docker)

**Produces:**
- `position.State{Quantity, AvgEntryPrice, RealizedPnL decimal.Decimal; LastVenueSequence int64}`
- `position.Apply(s State, e ledger.Event) (State, error)`

This is the heart of the milestone, and it is a unit-testable pure function (L4).

- [ ] Write the failing table-driven tests. The cases that matter:
      - two buys give the quantity-weighted average entry price
      - a partial sell realizes PnL on the sold quantity and leaves the average entry
        price unchanged (K5)
      - a sell larger than the position flips it: PnL is realized on the closed quantity
        only, and the remainder opens short at the trade price
      - a fee paid in BNB moves the fee component of realized PnL and never touches
        `AvgEntryPrice` (L9, K18)
      - closing to exactly zero leaves `AvgEntryPrice` at zero, not at the last price
      - an event at or below `LastVenueSequence` is rejected, so the fold cannot silently
        replay an event it has already seen
- [ ] Run and watch them fail.
- [ ] Implement. Every intermediate value is `decimal.Decimal`; the L1 guard test in
      `internal/store` is extended to cover `internal/position` as well.
- [ ] Tests green, lint clean, commit.

---

## Task 5: The projector and the invariant tests

**Files:** `migrations/00007_positions.sql`, `internal/position/project.go`
**Test:** `internal/position/project_integration_test.go`

**Produces:**
- `position.Project(ctx, db tenancy.Beginner, accountID, integrationID uuid.UUID) error`
- `position.Rebuild(ctx, db, accountID, integrationID uuid.UUID) error`

- [ ] Write the failing invariant tests (`ARCHITECTURE.md` §12, `PROJECT.md` §9):
      1. **Idempotency** -- projecting the same events twice leaves `positions` unchanged
      2. **Order independence** -- shuffling the ingest order gives a byte-identical
         projection, because the fold reads in canonical order (L7)
      3. **Rebuild equality** -- drop `positions`, fold from zero, compare field by field
         with `go-cmp`. There is no acceptable delta (L3)
      4. **A strategy assignment survives a rebuild** (D2). This is the test that would
         have caught the schema as documented
- [ ] Run and watch them fail.
- [ ] Migration and projector. The cursor is `positions.last_venue_sequence`, per
      integration, never the global `seq` (L6, K20).
- [ ] Tests green, lint clean, commit.

---

## Task 6: Golden fixture replay

**Files:** `testdata/golden/*.json`
**Test:** `internal/position/golden_test.go` (unit)

- [ ] Write the golden fixtures as canonical events (D3), with the hand-computed expected
      state in the same file so a reviewer can check the arithmetic without running
      anything.
- [ ] Replay each through `Apply` and compare with `go-cmp`.
- [ ] Mark M1 done in `docs/PROJECT.md` §8 and record D1-D3 in `docs/DECISIONS.md` as
      K29-K31, so the reasoning does not live only in this plan.
- [ ] Full Definition of Done, then hand to Codex.

---

## Verification

| Exit criterion | Proven by |
|---|---|
| Spot average cost and realized PnL correct | Task 6 golden replay |
| Time-scoped alias resolution | Task 1 and Task 2 recycle-boundary tests |
| Idempotency | Task 5 test 1 |
| Order independence | Task 5 test 2 |
| Rebuild equality | Task 5 test 3 |
| Identity excludes source (L5) | Task 3 dedup-across-sources test |
| The ledger is append-only (L2) | Task 3 privilege test |
| No `float64` on a money path (L1) | the generated-types guard, extended to `position` |

**Highest-value review targets for Codex:** the flip case in `position.Apply`, the
exclusion constraints on both alias tables, and whether `Project` can lose an event when
it runs concurrently with an append.
