# M7.5 — Scenario Shock Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Answer the only question a leveraged trader asks that the rest of the system cannot:
*what happens to me if the price moves?*

**Architecture:** One pure engine and one endpoint. `scenario` is a function of
`(wallet balance, positions, holdings, shocks)` producing a before and an after — equity,
margin balance, maintenance requirement and buffer. It is cheap because every engine it
builds on is already pure (L4): nothing here needs a database, a clock or a network.

**Tech Stack:** Go 1.26 · shopspring/decimal · Huma v2 · Next.js

**Spec:** `docs/PROJECT.md` §8 (M7.5 row) · `docs/ARCHITECTURE.md` §9 ·
`docs/DECISIONS.md` K13 K50 K56 · `docs/BINANCE-API-NOTES.md` F15 · `CLAUDE.md` L1 L4 L11

---

## Context

M5 wrote `collateral.MaintenanceAt` with a comment saying it exists because M7.5 needs it,
and naming the trap: **a shock large enough to matter crosses into a higher maintenance tier**,
so scaling today's maintenance margin by the price move silently assumes a constant rate and
is wrong in the direction that understates the danger. This milestone is that function's
first caller.

Three properties decide whether the answer is worth reading, and each is recorded as part of
K56.

### K56 — A shock names its asset, and the unshocked hold still

A scenario is a statement about specific assets. "BTC minus twenty percent" must not move ETH,
because inventing a correlation the user did not ask for produces a number that looks like
analysis and is a guess.

The corollary is the part that matters most: a shock hits **the spot holding and the perpetual
together**, because they are the same asset. A user long spot BTC and short BTC perp is
hedged, and a scenario that moved only one leg would report a loss they do not have — which is
precisely the false alarm K13 exists to prevent, arriving through a different door.

### The asymmetry between what may be missing

An unpriced holding and a missing bracket table are both absences, and they are **not** handled
the same way, because they fail in opposite directions:

- **An unpriced holding is excluded from equity.** That understates equity, which overstates
  the danger. Safe, and named.
- **A missing bracket table makes the buffer unavailable entirely.** Summing the maintenance
  requirements we happen to know understates the requirement, which *overstates* the buffer —
  it reports an account as safer than it is, at the moment it is being asked whether it is safe.

Partial answers are allowed where they are conservative and forbidden where they flatter.

---

## Global Constraints

- **L1** — a shock is a decimal fraction, not a float. `-0.2` is minus twenty percent, carried
  as `NUMERIC`/`decimal.Decimal`/JSON string end to end.
- **L4** — `scenario.Project` is pure. Prices, brackets and the shocks are inputs.
- **L11** — anything the projection could not compute is named, never absorbed into a total.
- **L13** — this endpoint models. It never places an order, and it writes nothing at all.
- A price cannot go below zero: a move at or below `-1` is refused rather than clamped.

---

## Task 1: The engine — pure

**Files:** `backend/internal/scenario/scenario.go`, `scenario_test.go`

- [ ] **Step 1:** the failing tests — the hedge case first, then the tier crossing, then the
  two asymmetric absences, then the refusal of a move below -1.
- [ ] **Step 2:** run to verify failure.
- [ ] **Step 3:** implement `Project(Input) (Report, error)`.
- [ ] **Step 4:** run; **Step 5:** mutation-test; **Step 6:** commit.

## Task 2: The endpoint and the page

**Files:** `backend/internal/httpapi/scenario.go`, `backend/internal/portfolio/scenario.go`,
`frontend/app/risk/page.tsx`

- [ ] `POST /risk/scenario`, reading the same stored snapshot `/risk` reads, so the base case
  in the response is the number the user is already looking at.
- [ ] Every gate, then the PR.
