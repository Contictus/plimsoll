# Binance API — verified notes

**Verified against the official documentation on 2026-09-03.** Every figure below was read
from `developers.binance.com`, not from memory and not from a blog post. `CLAUDE.md` §2
requires this because getting these details wrong produces plausible, wrong numbers.

Re-verify before M2 lands: Binance changes endpoint versions and weights without notice,
and two of the facts below already contradict what the ecosystem still documents widely.

| Source | URL |
|---|---|
| Spot REST | https://developers.binance.com/docs/binance-spot-api-docs/rest-api |
| Spot user data | https://developers.binance.com/docs/binance-spot-api-docs/user-data-stream |
| Spot WS API user data | https://developers.binance.com/docs/binance-spot-api-docs/websocket-api/user-data-stream-requests |
| Spot limits | https://developers.binance.com/docs/binance-spot-api-docs/rest-api/limits |
| USD-M user data | https://developers.binance.com/docs/derivatives/usds-margined-futures/user-data-streams |
| USD-M trades | https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/Account-Trade-List |
| USD-M income | https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Get-Income-History |
| USD-M position risk | https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/Position-Information-V3 |
| Wallet deposits | https://developers.binance.com/docs/wallet/capital/deposite-history |
| API key permission | https://developers.binance.com/docs/wallet/account/api-key-permission |

---

## 1. The three findings that change M2

### F1 — Spot `listenKey` is gone

Following the 2025-04-07 announcement, listenKey documentation for
`wss://stream.binance.com` was **removed**. Spot user data now arrives by subscribing on
the **WebSocket API**:

- `userDataStream.subscribe` — requires an authenticated connection established with
  `session.logon`, and **that requires Ed25519 keys**
- `userDataStream.subscribe.signature` — the per-request signature variant, usable with a
  normal HMAC key
- 2 IP weight per subscribe request
- `session.subscriptions` lists what is active; `userDataStream.unsubscribe` ends one

**Consequence for K25:** the credential model has to hold an Ed25519 private key, not only
an HMAC secret, if we want the authenticated path. The signature variant avoids that and
is the smaller change. This is a decision M2's plan has to take explicitly.

**USD-M futures still uses listenKey**, so the two markets do not share a realtime path:

| | Spot | USD-M |
|---|---|---|
| Mechanism | WS API `userDataStream.subscribe` | `listenKey` |
| Start | — | `POST /fapi/v1/listenKey` (weight 1) |
| Keepalive | — | `PUT /fapi/v1/listenKey` (weight 1) |
| Close | `userDataStream.unsubscribe` | `DELETE /fapi/v1/listenKey` (weight 1) |
| Expiry | not documented on the page read | 60 minutes without a keepalive |

The docs suggest pinging "about every 60 minutes", which is the expiry itself — ping well
inside it. Every listenKey call needs the `X-MBX-APIKEY` header.

### F2 — Futures history stops at three months

Both `/fapi/v1/userTrades` and `/fapi/v1/income` state it plainly:

> "Only support querying trade in the past 3 months."
> "Income history only contains data for the last three months."

**The ledger therefore cannot be complete for USD-M before that date, ever, from these
endpoints.** This is not a bug to fix; it is a fact to report. L11 says degraded and
visible beats confident and wrong, and the M0 reason-code set has nothing for it —
`backfill_incomplete` says "not finished yet", which is a different and recoverable claim.
M2 needs a distinct reason, e.g. `history_truncated`, meaning "the venue will not give us
this and never will".

### F3 — `tranId` is unique per `incomeType`, not globally

> "tranId is unique in the same incomeType for a user."

So an income event's identity **must** carry the type:
`usdm:income:FUNDING_FEE:98765432`. This is exactly the shape `ARCHITECTURE.md` §3 already
documents — now confirmed from the source rather than assumed. Dropping `incomeType` from
the key would collide a funding fee with a commission and silently deduplicate one away
(L5, K19).

### F4 — There is no "all my spot trades" endpoint

`GET /api/v3/myTrades` requires `symbol`. So does `allOrders`. The only account endpoint
that accepts an omitted symbol is `openOrders` (weight 6 with a symbol, 80 without), and
open orders are not history.

`GET /sapi/v1/accountSnapshot` does not rescue this either: **IP weight 2400**, and it
"supports query within the last one month only". It is not a discovery tool, it is a
liability.

**So symbol discovery has to be solved by us, and it decides how M2's backfill is shaped.**
An asset that was bought and fully sold leaves no trace in current balances and none in
deposit or withdrawal history — the only record is its trades, which cannot be found
without already knowing the symbol. Any discovery built from balances and transfers has
that hole in it, and K26 exists precisely to refuse holes.

The approach that has none: probe **every** spot symbol from `exchangeInfo` once, with
`myTrades?symbol=X&limit=1`. Roughly 2–3k symbols at weight 20 is 40–60k weight — under
ten minutes of a dedicated budget, once, for a full-history import. Symbols that come back
empty are never queried again.

### F5 — The 24-hour window binds `startTime`/`endTime`, not `fromId`

The constraint is written as "the time between `startTime` and `endTime` can't be longer
than 24 hours" — it is a constraint on the pair. The `fromId` path carries no documented
time restriction and returns trades with id ≥ the value given.

So a spot symbol's whole history is walked with `fromId` and `limit=1000`, one weight-20
request per thousand trades, with no time chunking at all. The 24-hour window then matters
only for the gap-resync path, where the window is known and small.

**Confidence:** this is read from the phrasing, not from a sentence that states it
outright. It is the first thing M2 verifies when it records its fixtures, and the backfill
design falls back to 24-hour chunks if it turns out to be wrong.

---

## 2. Endpoints M2 needs

| Endpoint | Weight | Limit | Time window | History depth |
|---|---|---|---|---|
| `GET /api/v3/myTrades` | **20** (5 with `orderId`) | max 1000, default 500 | **≤ 24 hours** | not stated |
| `GET /api/v3/account` | 20 | — | — | — |
| `GET /fapi/v1/userTrades` | 5 | max 1000, default 500 | **≤ 7 days** | **3 months** |
| `GET /fapi/v1/income` | **30** | max 1000, default 100 | default 7 days | **3 months** |
| `GET /fapi/v3/positionRisk` | 5 | — | — | — |
| `GET /sapi/v1/capital/deposit/hisrec` | 1 | max 1000, default 1000 | **≤ 90 days** | — |
| `GET /sapi/v1/capital/withdraw/history` | 1 | max 1000 | **≤ 90 days** | — |
| `GET /api/v3/exchangeInfo` | see docs | — | — | — |
| `GET /sapi/v1/accountSnapshot` | **2400** | 7–30 snapshots | — | **1 month** |
| `POST/PUT/DELETE /fapi/v1/listenKey` | 1 | — | — | — |

Pagination differs by endpoint and the differences matter:

- **`myTrades`** — `fromId` returns trades with id ≥ that value. Without it, the most
  recent are returned.
- **`userTrades`** — `fromId` **cannot be sent together with** `startTime`/`endTime`. So a
  chunked historical walk and an id-based walk are two different strategies, not one with
  an optional parameter.
- **`income`** — `page`/`limit`, not `fromId`. Offset pagination over a moving window is
  the one that can skip or repeat rows if the window shifts underneath it; the chunk
  boundaries have to be pinned by time, not by page.
- **deposits/withdrawals** — `offset`/`limit`.

`positionRisk` is **V3** (`/fapi/v3/positionRisk`), not the V2 most examples still show.
K6 reads the exchange's liquidation price from here rather than computing it, so the
version and its field names are load-bearing.

## 3. Rate limits

Exact header names, quoted from the docs:

```
X-MBX-USED-WEIGHT-(intervalNum)(intervalLetter)
X-MBX-ORDER-COUNT-(intervalNum)(intervalLetter)
Retry-After
```

Interval letters are `S` / `M` / `H` / `D`. So the header to read on a normal request is
`X-MBX-USED-WEIGHT-1M`.

- **429** — a rate limit was broken; `Retry-After` says how long to wait.
- **418** — the IP was auto-banned for continuing to send after 429s. Bans scale from
  **2 minutes to 3 days** for repeat offenders.

> "Limits are based on the IPs, not the API keys."

**This confirms K24.** Two integrations belonging to two different accounts, running from
one server, share one budget. A per-key limiter alone would let one account's backfill get
another account's IP banned for three days — which is why K24 is two-tier: per-integration
weight accounting plus a shared per-IP gate.

The per-minute weight ceiling is **not hardcoded**: `/api/v3/exchangeInfo` returns a
`rateLimits` array with `RAW_REQUESTS`, `REQUEST_WEIGHT` and `ORDERS`. Read it at connect
time. A hardcoded number is a number that will be wrong after Binance changes it and we
will not notice until the ban.

## 4. Read-only key verification (K9, L13)

> **Corrected 2026-09-03** while writing M2 task 2. The earlier version of this section
> listed six fields and asserted that `enableFutures` is a read permission. Both were
> wrong: the documented response carries **thirteen** fields, and the page states no
> semantics for any of them. The paragraph below replaces it.

`GET /sapi/v1/account/apiRestrictions` — weight 1 (IP), requires the `X-MBX-APIKEY` header
and a `timestamp`; `recvWindow` is optional and capped at 60000. The response example given
on the page, verbatim and complete:

```json
{
  "ipRestrict": false,
  "createTime": 1623840271000,
  "enableReading": true,
  "enableWithdrawals": false,
  "enableInternalTransfer": true,
  "enableMargin": false,
  "enableFutures": false,
  "permitsUniversalTransfer": true,
  "enableVanillaOptions": false,
  "enableFixApiTrade": false,
  "enableFixReadOnly": true,
  "enableSpotAndMarginTrading": false,
  "enablePortfolioMarginTrading": true
}
```

**The page documents no meaning for any field.** Neither does the account-management FAQ,
which says only that permissions beyond reading should not be enabled without an IP
restriction, and that withdrawals require one. So the semantics of `enableFutures`,
`enableInternalTransfer` and `permitsUniversalTransfer` are **not** verified, and the
previous claim that `enableFutures` is a read permission had no source behind it.

That unresolved question decides the rule rather than blocking it. K9 says an
over-permissioned key is rejected, and under uncertainty the safe reading of "permission"
is the broad one. So `integration.Verify` **allows only a known list of read permissions to
be true** — `enableReading` and `enableFixReadOnly` — and rejects every other boolean that
is true, named or not. A field Binance adds next year is rejected by default, which is the
only version of this check that cannot silently start accepting a trading key.

Three fields are explicitly **not** capabilities and never cause a rejection: `createTime`
(a timestamp), `tradingAuthorityExpirationTime` (a timestamp; absent from the example but
documented elsewhere), and `ipRestrict` — which is a *restriction*, so `true` is the safer
key and rejecting it would be exactly backwards.

The cost of the broad reading is that a user whose key also has futures enabled is asked to
issue a separate key for us. That is the correct trade in M2, which is spot-only. **M5 must
resolve `enableFutures` against a real response before it can read futures data**, and if it
turns out to permit order placement then it stays rejected and futures reading needs a
different answer.

## 5. What is still unverified

> **Decided 2026-09-04.** Contact with a real Binance account is deferred as far as the
> project allows, so nothing below will be settled by recording a payload in the near term.
> Where an item shapes code, the code takes the defensive branch and says so; it does not
> wait. `plimsollctl record` can settle any of these in one command if a key ever appears.

**F5 is the one that shapes M2.** What the documentation *does* state is that `fromId`
fetches from a trade id and that the 24-hour limit binds `startTime`/`endTime`. What it does
not state is what `fromId=0` with no time range returns. The plan inferred "the oldest
trades"; the inference is not verified.

Abandoning `fromId` is not the safe alternative — a pure 24-hour walk from spot's 2017 launch
is roughly 3,300 windows per symbol at weight 20, which is not a backfill anyone runs. So
task 6 keeps `fromId` and **checks the inference at runtime instead of assuming it**: if the
first page returned for `fromId=0` is not contiguous with the pages that follow, the walk
stops and raises `backfill_incomplete` in `freshness` (L11) rather than reporting a history
it has silently truncated. Degraded and visible beats confident and wrong, and this costs
nothing to build.


Recorded so the M2 plan does not quietly assume them:

- The exact spot `REQUEST_WEIGHT` ceiling per minute — read from `exchangeInfo` instead.
- Whether spot trade history has a depth limit comparable to the futures three months.
- Whether `enableFutures` grants futures **trading** or only futures **reading**. Neither
  the API-key-permission page nor the account FAQ says. M2 rejects it either way; M5
  cannot proceed on futures without an answer. See §4.
- Whether `enableInternalTransfer` and `permitsUniversalTransfer` can move funds off the
  account or only between the user's own wallets. Rejected either way for the same reason.
- The complete `incomeType` enum. Eight were listed on the page read
  (`TRANSFER`, `WELCOME_BONUS`, `REALIZED_PNL`, `FUNDING_FEE`, `COMMISSION`,
  `INSURANCE_CLEAR`, `REFERRAL_KICKBACK`, `COMMISSION_REBATE`) and "14 additional types"
  were not enumerated. The normalizer must reject an unknown type loudly rather than map
  it to something plausible.
- USD-M user data event payloads (`ACCOUNT_UPDATE`, `ORDER_TRADE_UPDATE`) and the futures
  websocket base URL.
- Whether the spot WS API subscription expires, and what keeps it alive.
- SBE versus JSON. SBE is offered; JSON is the one whose payload we can store verbatim in
  `raw` and read six months later (L15), which is an argument on its own.
