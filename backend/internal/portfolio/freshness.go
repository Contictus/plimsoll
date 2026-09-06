package portfolio

import (
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/google/uuid"
)

// ReasonsFor turns what is known about an account's ingestion into the reasons its response
// must carry. Pure, and separate from the loader, because the ranking of these conditions is
// a product decision that deserves tests of its own rather than a database (L4, L11).
//
// About Since: it is when the condition is known to have started. Where that is knowable it
// is used -- the state's own timestamp, or when a silent worker last spoke. Where it is not,
// it is the read time, which claims only "this was true when we looked" and never that the
// condition just began.
func ReasonsFor(
	statuses []ingest.Status,
	lagging map[uuid.UUID]bool,
	now time.Time,
	leaseTTL time.Duration,
) []freshness.Reason {
	// Always first, and always present until M4: there is no total in this response, and a
	// response that merely omitted one would leave a client to guess whether the account
	// holds nothing or nothing was priced.
	reasons := []freshness.Reason{{
		Code:     freshness.ReasonValuationUnavailable,
		Severity: freshness.SeverityWarn,
		Detail: "no price source has run: totals are subtotals per quote asset, and nothing" +
			" here is marked to market",
		Since: now,
	}}

	for _, s := range statuses {
		name := fmt.Sprintf("%s %s", s.Exchange, s.Label)

		if s.Stale(now, leaseTTL) {
			reasons = append(reasons, stalled(s, name, now))
			continue
		}
		if reason, degraded := s.State.Reason(); degraded {
			reason.Detail = fmt.Sprintf("%s: %s", name, reason.Detail)
			reason.Since = s.Since
			reasons = append(reasons, reason)
		}
	}

	for _, s := range statuses {
		if !lagging[s.IntegrationID] {
			continue
		}
		reasons = append(reasons, freshness.Reason{
			Code:     freshness.ReasonProjectionLagging,
			Severity: freshness.SeverityWarn,
			Detail: fmt.Sprintf("%s %s: events are in the ledger that the fold has not"+
				" reached; these positions are behind them", s.Exchange, s.Label),
			Since: now,
		})
	}
	return reasons
}

// stalled describes an integration nothing is currently reading. It is not ws_gap: that is
// a worker which is running and telling you its feed is down, and this is the absence of a
// worker to tell you anything -- which is why no worker can raise it.
//
// An integration the user paused is expected rather than broken, so it warns instead of
// erroring. Reporting the deliberate case at the same severity as an active integration
// nobody is running would make the loud one indistinguishable from the quiet one.
func stalled(s ingest.Status, name string, now time.Time) freshness.Reason {
	severity := freshness.SeverityWarn
	detail := fmt.Sprintf("%s is %s and no worker is reading it", name, s.Configured)
	if s.Configured == "active" {
		severity = freshness.SeverityError
		detail = fmt.Sprintf("%s: no worker is reading this integration, so trades happening"+
			" now are not being recorded", name)
	}

	since := now
	if s.Reported {
		// When it last spoke, not when we noticed it had stopped. The difference is the
		// whole of what an operator needs to know.
		since = s.UpdatedAt
		detail = fmt.Sprintf("%s: the worker stopped reporting", name)
		if s.Configured != "active" {
			detail = fmt.Sprintf("%s is %s and its worker stopped reporting", name, s.Configured)
		}
	}
	return freshness.Reason{Code: freshness.ReasonIngestStalled, Severity: severity, Detail: detail, Since: since}
}
