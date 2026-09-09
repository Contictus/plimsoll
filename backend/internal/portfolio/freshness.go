package portfolio

import (
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/google/uuid"
)

// ValuationUnavailable means no run has completed, and nothing weaker. M4 narrowed it from
// "no price engine exists" to exactly this, and did not retire it: on a fresh install whose
// feed has never connected there genuinely is no valuation, and a response that dropped the
// reason would report a confident zero -- the failure the reason was written to prevent,
// arriving through the door marked cleanup.
//
// A warning and not an error: every number present is exact, and marking an exact response
// unreliable erodes what status means just as surely as failing to mark a wrong one.
func ValuationUnavailable(now time.Time) freshness.Reason {
	return freshness.Reason{
		Code:     freshness.ReasonValuationUnavailable,
		Severity: freshness.SeverityWarn,
		Detail: "no valuation run has completed: totals are subtotals per quote asset, and" +
			" nothing here is marked to market",
		Since: now,
	}
}

// ReasonsFor turns what is known about an account's ingestion into the reasons any response
// built on it must carry. Pure, and separate from the loader, because the ranking of these
// conditions is a product decision that deserves tests of its own rather than a database
// (L4, L11).
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
	reasons := make([]freshness.Reason, 0, len(statuses)+1)

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

// NegativeBalances reports every asset the ledger says the account holds less than nothing
// of. It is K14's strongest check: it finds a missing event without knowing what the
// missing event was, because an account cannot sell what it never had.
//
// An error, not a warning. A balance that cannot exist means the numbers built on it are
// wrong rather than merely late, and the difference between those two is the whole reason
// severity exists.
//
// One reason per asset, and the asset is named. "Something is negative" is not actionable;
// "you are 0.4 BTC short on binance/main" is where an operator starts.
func NegativeBalances(balances []Balance) []freshness.Reason {
	var out []freshness.Reason
	for _, b := range balances {
		if !b.Quantity.IsNegative() {
			continue
		}
		out = append(out, freshness.Reason{
			Code:     freshness.ReasonNegativeBalance,
			Severity: freshness.SeverityError,
			Detail: fmt.Sprintf(
				"the ledger implies holding %s %s, which cannot be true: an event is missing",
				b.Quantity.String(), b.Asset),
			// When it first went negative is not knowable without replaying, and this
			// endpoint does not replay. LastEventTime is the honest nearest thing: the
			// balance has been what it is at least since then.
			Since: b.LastEventTime,
		})
	}
	return out
}

// UnattributedFees reports integrations holding a fee whose asset never resolved.
//
// The fee was stored -- losing a fill to keep a fee would be the worse trade (K22) -- so
// the balance for that asset is short by exactly those amounts. A warning rather than an
// error: the shortfall is bounded by the fees themselves, and every other number is intact.
func UnattributedFees(
	statuses []ingest.Status, unattributed map[uuid.UUID]bool, now time.Time,
) []freshness.Reason {
	var out []freshness.Reason
	for _, s := range statuses {
		if !unattributed[s.IntegrationID] {
			continue
		}
		out = append(out, freshness.Reason{
			Code:     freshness.ReasonUnknownSymbol,
			Severity: freshness.SeverityWarn,
			Detail: fmt.Sprintf(
				"%s %s: a fee was paid in a ticker the asset registry does not cover, so the"+
					" balance for it is short by that fee", s.Exchange, s.Label),
			Since: now,
		})
	}
	return out
}

// AssumedPeg discloses that a leg of the run fell back to a peg instead of a traded price
// (K17). Info, not warn: the assumption is named, every other number is arithmetic on real
// prices, and spending a warning on the ordinary case is how a client learns to ignore them.
//
// It is deliberately not conditional on whether the account holds the assumed asset. The
// total is built from one run, and if that run leaned on an assumption anywhere the reader
// is entitled to know before comparing it against an exchange screen.
func AssumedPeg(since time.Time) freshness.Reason {
	return freshness.Reason{
		Code:     freshness.ReasonAssumedPeg,
		Severity: freshness.SeverityInfo,
		Detail: "a leg of this valuation assumed a stablecoin peg rather than a traded" +
			" price, so the total carries that assumption",
		Since: since,
	}
}

// PriceStale reports the age of the run's worst leg. The worst and not the average: a total
// is only as current as the oldest price inside it, and an average would hide one forgotten
// instrument behind a hundred fresh ones.
//
// The total is still served. A stale price is an answer with a caveat; withholding it would
// leave the reader with nothing, and silence is the worst possible failure (L11).
func PriceStale(observedAt time.Time, age time.Duration) freshness.Reason {
	return freshness.Reason{
		Code:     freshness.ReasonPriceStale,
		Severity: freshness.SeverityWarn,
		Detail: fmt.Sprintf("the oldest price in this valuation is %s old; the total is"+
			" marked to a market that has moved since", age.Round(time.Second)),
		Since: observedAt,
	}
}

// FeePriceMissing reports a fee paid in an asset the run could not price. The fee totals
// stay exact and stay in their own asset (L9); what is missing is only the conversion, which
// is why it warns rather than errors -- every other number in the response is intact.
func FeePriceMissing(asset string, since time.Time) freshness.Reason {
	return freshness.Reason{
		Code:     freshness.ReasonFeePriceMissing,
		Severity: freshness.SeverityWarn,
		Detail: fmt.Sprintf("fees were paid in %s, which this valuation could not price, so"+
			" they are reported in %s and are not in any total", asset, asset),
		Since: since,
	}
}
