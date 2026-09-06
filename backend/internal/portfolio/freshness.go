package portfolio

import (
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/google/uuid"
)

// ValuationUnavailable is what a response carrying numbers says while M4 does not exist:
// there is no total, only subtotals per quote asset. A response that merely omitted the
// total would leave a client to guess whether the account is empty or nothing was priced.
//
// Only responses that would otherwise carry a total raise it. A ledger listing values
// nothing, and adding it there would be noise -- and noise in freshness erodes it exactly
// as fast as silence does.
func ValuationUnavailable(now time.Time) freshness.Reason {
	return freshness.Reason{
		Code:     freshness.ReasonValuationUnavailable,
		Severity: freshness.SeverityWarn,
		Detail: "no price source has run: totals are subtotals per quote asset, and nothing" +
			" here is marked to market",
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
