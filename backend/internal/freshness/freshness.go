// Package freshness is the vocabulary in which a response admits what it is missing: the
// closed set of reason codes, their severities, and the envelope every data response
// carries (K23, L10, L11).
//
// It is its own package, and not part of httpapi, because the packages that *raise* a
// reason are not the package that serves it. The ingestion state enum and the portfolio
// read model both need this vocabulary, and httpapi needs both of them -- so leaving the
// types in httpapi makes an import cycle out of what is really one shared contract.
package freshness

import "time"

// Severity ranks how badly a Reason undermines a response. The order matters: Status is
// derived from the worst one present.
type Severity string

// The three severities, least to most serious.
const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Status is the single field a caller can read to decide whether to trust a response.
type Status string

// The three statuses a response can carry.
const (
	StatusOK         Status = "ok"
	StatusDegraded   Status = "degraded"
	StatusUnreliable Status = "unreliable"
)

// The closed set of reason codes (ARCHITECTURE.md §5). They are constants rather than
// free strings so a new degradation has to be named here, where a client contract change
// is visible, instead of appearing in a response as a typo nobody can match on.
const (
	ReasonWSGap              = "ws_gap"
	ReasonPriceStale         = "price_stale"
	ReasonBackfillIncomplete = "backfill_incomplete"

	// ReasonHistoryTruncated is deliberately not ReasonBackfillIncomplete. "Incomplete"
	// means not finished yet -- recoverable, and it implies waiting will fix it. This one
	// says the venue will never return data before a cutoff, which is a permanent claim
	// about what can be known. Telling a user to wait for something that will not arrive is
	// the confident-and-wrong failure L11 exists to reject.
	ReasonHistoryTruncated = "history_truncated"

	ReasonAssumedPeg             = "assumed_peg"
	ReasonUnknownSymbol          = "unknown_symbol"
	ReasonReconciliationMismatch = "reconciliation_mismatch"
	ReasonFeePriceMissing        = "fee_price_missing"

	// ReasonValuationUnavailable means no price source has run, so the response carries no
	// total -- only subtotals per quote asset. It is a warning and not an error on purpose:
	// every number present is exact, and marking an exact response unreliable erodes what
	// `status` means just as surely as failing to mark a wrong one.
	ReasonValuationUnavailable = "valuation_unavailable"

	// ReasonIngestStalled means nothing is currently reading this integration: no worker has
	// ever reported on it, or the one that did has stopped saying anything. Distinct from
	// ws_gap, which is a worker that is running and telling you its feed is down. This one
	// is the absence of a reporter, which is why it cannot be raised by a worker.
	ReasonIngestStalled = "ingest_stalled"

	// ReasonProjectionLagging means events are in the ledger that the fold has not reached,
	// so the positions in this response are behind the events that produced them. Expected
	// briefly and constantly -- the fold runs on a ticker (K38) -- and a warning rather than
	// an error because the shortfall is bounded by that tick and closes on its own.
	ReasonProjectionLagging = "projection_lagging"
)

// Reason explains one way in which a response is less than fully current. Detail is for a
// human reading a dashboard; Code is what a client matches on.
type Reason struct {
	Code     string    `json:"code"     doc:"machine-readable reason code"`
	Severity Severity  `json:"severity" enum:"info,warn,error"`
	Detail   string    `json:"detail"`
	Since    time.Time `json:"since"    doc:"when this condition started"`
}

// Report replaces the boolean `stale` (K23). It is API surface, not diagnostics:
// degraded and visible always beats confident and wrong (L11).
type Report struct {
	Status  Status   `json:"status" enum:"ok,degraded,unreliable"`
	Reasons []Reason `json:"reasons"`
}

// Envelope is embedded in every data response so that as_of and freshness cannot be
// forgotten on a new endpoint (L10, L11). AsOf is the valuation run's instant, not the
// time the response was serialized.
type Envelope struct {
	AsOf      time.Time `json:"as_of"`
	Freshness Report    `json:"freshness"`
}

// New derives Status from the worst severity present, so a caller reading only
// Status is never misled by an error buried under later warnings. Reasons is always a
// list, never nil, so a client can iterate it without a nil check.
func New(reasons ...Reason) Report {
	out := Report{Status: StatusOK, Reasons: make([]Reason, 0, len(reasons))}
	for _, r := range reasons {
		out.Reasons = append(out.Reasons, r)
		switch r.Severity {
		case SeverityError:
			out.Status = StatusUnreliable
		case SeverityWarn:
			// Only ever an upgrade: a warning arriving after an error must not talk the
			// response back up to merely degraded.
			if out.Status == StatusOK {
				out.Status = StatusDegraded
			}
		case SeverityInfo:
			// Informational by definition -- an assumed peg is disclosed, not a fault.
		}
	}
	return out
}

// ReasonLineageMismatch means replaying a position's events did not reproduce the stored
// projection, on the same last event. It is the one reason in this set that accuses the
// system itself rather than the venue or the network, and it is an error without
// qualification: the whole product claim is that the numbers are right and we can prove it,
// so a proof that comes out different is the most serious thing an API can report.
const ReasonLineageMismatch = "lineage_mismatch"

// ReasonNegativeBalance means the ledger implies holding less than nothing of an asset --
// selling more than was ever acquired. It is K14's strongest check because it finds a
// missing event without knowing what the missing event was, and it is an error: a balance
// that cannot exist means the numbers built on it are wrong, not merely late.
const ReasonNegativeBalance = "negative_balance"

// ReasonCollateralStale means the account's margin picture is older than a capture interval:
// the buffer and the liquidation distances below it describe the exchange as it was, not as
// it is. It is served rather than withheld -- an hour-old distance is still the best answer
// anyone has -- and it is a warning, because the numbers are exact about a moment that has
// passed rather than wrong about this one.
const ReasonCollateralStale = "collateral_stale"

// ReasonCollateralUnavailable means no capture exists for an integration that should have
// one, so how close it is to liquidation is unknown.
//
// An error, and the response omits the integration entirely rather than rendering it with
// zeroes. A margin buffer of zero and an unknown margin buffer are opposite claims -- one
// says liquidation is imminent, the other says nothing at all -- and a screen that renders
// them the same is the confident-and-wrong failure at the one moment it costs most (L11).
const ReasonCollateralUnavailable = "collateral_unavailable"
