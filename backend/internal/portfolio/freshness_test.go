package portfolio_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const testLeaseTTL = 2 * time.Minute

func codes(reasons []freshness.Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, r.Code)
	}
	return out
}

func reasonWith(t *testing.T, reasons []freshness.Reason, code string) freshness.Reason {
	t.Helper()
	for _, r := range reasons {
		if r.Code == code {
			return r
		}
	}
	t.Fatalf("no %s in %v", code, codes(reasons))
	return freshness.Reason{}
}

func live(id uuid.UUID, at time.Time) ingest.Status {
	return ingest.Status{
		IntegrationID: id, Exchange: "binance", Label: "main", Configured: "active",
		Reported: true, State: ingest.StateLive, OwnerID: "w1", Since: at, UpdatedAt: at,
	}
}

// M4 owns prices. Until then there is no total, and a response that just omitted one would
// leave a client to guess whether the account holds nothing or nothing was priced.
//
// Warn, not error: the subtotals present are exact. Marking an exact response unreliable
// erodes what status means as surely as failing to mark a wrong one.
func TestValuationUnavailableIsAlwaysPresentAndOnlyDegrades(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	reasons := portfolio.ReasonsFor(nil, nil, now, testLeaseTTL)

	require.Equal(t, []string{freshness.ReasonValuationUnavailable}, codes(reasons))
	require.Equal(t, freshness.SeverityWarn,
		reasonWith(t, reasons, freshness.ReasonValuationUnavailable).Severity)
	require.Equal(t, freshness.StatusDegraded, freshness.New(reasons...).Status)
}

// An integration nobody has ever reported on is not "connecting". It is an integration
// nothing is reading, and that is the loudest thing a portfolio can have to say -- a query
// over the status table alone would not even see it.
func TestAnIntegrationWithNoWorkerIsReportedAsStalled(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	id := uuid.New()
	never := ingest.Status{
		IntegrationID: id, Exchange: "binance", Label: "main", Configured: "active",
	}

	reasons := portfolio.ReasonsFor([]ingest.Status{never}, nil, now, testLeaseTTL)

	stalled := reasonWith(t, reasons, freshness.ReasonIngestStalled)
	require.Equal(t, freshness.SeverityError, stalled.Severity)
	require.Contains(t, stalled.Detail, "main")
	require.Equal(t, freshness.StatusUnreliable, freshness.New(reasons...).Status)
}

// A worker that stopped saying anything is worse than any state it named. "Live, as of
// forty minutes ago" is the sentence this whole mechanism exists to keep out of a response.
func TestAWorkerThatStoppedReportingIsStalledRatherThanLive(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	id := uuid.New()
	stale := live(id, now.Add(-40*time.Minute))

	reasons := portfolio.ReasonsFor([]ingest.Status{stale}, nil, now, testLeaseTTL)

	got := reasonWith(t, reasons, freshness.ReasonIngestStalled)
	require.Equal(t, freshness.SeverityError, got.Severity)
	require.Equal(t, stale.UpdatedAt, got.Since,
		"since must be when it last spoke, not when we noticed it had stopped")
	require.NotContains(t, codes(reasons), freshness.ReasonWSGap)
}

// A paused integration with no worker is expected, not a fault. Reporting it at the same
// severity as an active one nobody is running would make the loud case indistinguishable
// from the deliberate one.
func TestAPausedIntegrationWithNoWorkerOnlyWarns(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	paused := ingest.Status{
		IntegrationID: uuid.New(), Exchange: "binance", Label: "old", Configured: "paused",
	}

	reasons := portfolio.ReasonsFor([]ingest.Status{paused}, nil, now, testLeaseTTL)

	require.Equal(t, freshness.SeverityWarn,
		reasonWith(t, reasons, freshness.ReasonIngestStalled).Severity)
}

// A live worker contributes nothing, which is the only way the reason list can ever be
// short enough to read.
func TestALiveWorkerAddsNoReasonOfItsOwn(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	reasons := portfolio.ReasonsFor([]ingest.Status{live(uuid.New(), now)}, nil, now, testLeaseTTL)

	require.Equal(t, []string{freshness.ReasonValuationUnavailable}, codes(reasons))
}

// A worker that is running and says its feed is down contributes its own state's reason.
// ingest_stalled and ws_gap are different claims: nobody is reading this, versus somebody
// is reading this and telling you it is broken.
func TestARunningWorkerContributesItsOwnState(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	degraded := live(uuid.New(), now)
	degraded.State = ingest.StateDegraded
	degraded.Since = now.Add(-90 * time.Second)

	reasons := portfolio.ReasonsFor([]ingest.Status{degraded}, nil, now, testLeaseTTL)

	got := reasonWith(t, reasons, freshness.ReasonWSGap)
	require.Equal(t, freshness.SeverityError, got.Severity)
	require.Equal(t, degraded.Since, got.Since, "since belongs to the state, not to the read")
	require.Contains(t, got.Detail, "main")
	require.NotContains(t, codes(reasons), freshness.ReasonIngestStalled)
}

// The fold runs on a ticker, so the positions can be behind the events that produced them.
// Bounded and self-closing, hence a warning -- but never silent (K38, L11).
func TestAProjectionBehindTheLedgerIsReported(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	id := uuid.New()

	reasons := portfolio.ReasonsFor([]ingest.Status{live(id, now)},
		map[uuid.UUID]bool{id: true}, now, testLeaseTTL)

	require.Equal(t, freshness.SeverityWarn,
		reasonWith(t, reasons, freshness.ReasonProjectionLagging).Severity)
}

// Two integrations in trouble are two reasons: collapsing them would make a response say
// "something is stalled" without saying which, and the label is the whole point.
func TestEachIntegrationSpeaksForItself(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	a := ingest.Status{IntegrationID: uuid.New(), Exchange: "binance", Label: "spot", Configured: "active"}
	b := ingest.Status{IntegrationID: uuid.New(), Exchange: "binance", Label: "perp", Configured: "active"}

	reasons := portfolio.ReasonsFor([]ingest.Status{a, b}, nil, now, testLeaseTTL)

	var stalled int
	for _, r := range reasons {
		if r.Code == freshness.ReasonIngestStalled {
			stalled++
		}
	}
	require.Equal(t, 2, stalled)
}
