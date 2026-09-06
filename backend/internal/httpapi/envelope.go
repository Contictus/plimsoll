// Package httpapi is the read-side HTTP surface: routing, the session cookie, and the
// envelope every data response carries. It never writes ledger events and never places an
// order (L13, ARCHITECTURE.md §10).
package httpapi

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
)

// Reason explains one way in which a response is less than fully current. Detail is for a
// human reading a dashboard; Code is what a client matches on.
type Reason struct {
	Code     string    `json:"code"     doc:"machine-readable reason code"`
	Severity Severity  `json:"severity" enum:"info,warn,error"`
	Detail   string    `json:"detail"`
	Since    time.Time `json:"since"    doc:"when this condition started"`
}

// Freshness replaces the boolean `stale` (K23). It is API surface, not diagnostics:
// degraded and visible always beats confident and wrong (L11).
type Freshness struct {
	Status  Status   `json:"status" enum:"ok,degraded,unreliable"`
	Reasons []Reason `json:"reasons"`
}

// Envelope is embedded in every data response so that as_of and freshness cannot be
// forgotten on a new endpoint (L10, L11). AsOf is the valuation run's instant, not the
// time the response was serialized.
type Envelope struct {
	AsOf      time.Time `json:"as_of"`
	Freshness Freshness `json:"freshness"`
}

// NewFreshness derives Status from the worst severity present, so a caller reading only
// Status is never misled by an error buried under later warnings. Reasons is always a
// list, never nil, so a client can iterate it without a nil check.
func NewFreshness(reasons ...Reason) Freshness {
	out := Freshness{Status: StatusOK, Reasons: make([]Reason, 0, len(reasons))}
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
