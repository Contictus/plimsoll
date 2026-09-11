package portfolio_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/stretchr/testify/require"
)

func openAt(kind, severity string, opened time.Time) quality.Stored {
	return quality.Stored{Kind: kind, Severity: severity, OpenedAt: opened}
}

var noon = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

// `since` answers "how long has this been true", so it is the OLDEST open finding. Taking the
// newest understates the age every time -- and a problem that has been true for a week is a
// different problem from one that started a minute ago, which is the entire reason the field
// exists.
func TestSinceIsTheOldestOpenFinding(t *testing.T) {
	got := portfolio.OpenFindings([]quality.Stored{
		openAt(quality.KindMissingEvent, quality.SeverityError, noon),
		openAt(quality.KindUnresolvedAsset, quality.SeverityWarn, noon.Add(-6*24*time.Hour)),
		openAt(quality.KindSnapshotFailed, quality.SeverityWarn, noon.Add(-time.Minute)),
	}, noon)

	require.Len(t, got, 1)
	require.True(t, got[0].Since.Equal(noon.Add(-6*24*time.Hour)),
		"six days, not one minute")
}

// A closed finding says nothing about the present. OpenFindings is fed from a query that
// already filters them, so this is the test that keeps the filter honest if that ever changes
// -- a reason raised by a problem that is over teaches the reader to ignore the banner.
func TestAClosedFindingRaisesNothing(t *testing.T) {
	closed := openAt(quality.KindMissingEvent, quality.SeverityError, noon)
	closed.ClosedAt = quality.ClosedAt{Time: noon.Add(time.Hour), Valid: true}

	require.Empty(t, portfolio.OpenFindings([]quality.Stored{closed}, noon))
}

// The reason takes the worst severity present, so a reader who looks at one field is not
// misled by a buried error (K23).
func TestTheReasonTakesTheWorstSeverityPresent(t *testing.T) {
	got := portfolio.OpenFindings([]quality.Stored{
		openAt(quality.KindSnapshotFailed, quality.SeverityWarn, noon),
		openAt(quality.KindMissingEvent, quality.SeverityError, noon),
	}, noon)

	require.Len(t, got, 1)
	require.Equal(t, freshness.SeverityError, got[0].Severity)
	require.Equal(t, freshness.ReasonReconciliationMismatch, got[0].Code)
}

// Rounding alone is info, and info is not doubt: spending a warning on the ordinary case is
// how a client learns to ignore them all.
func TestAnInfoOnlyRegisterRaisesNothing(t *testing.T) {
	require.Empty(t, portfolio.OpenFindings([]quality.Stored{
		openAt(quality.KindRounding, quality.SeverityInfo, noon),
	}, noon))
}

// An empty register is silence, not a reason saying nothing is wrong.
func TestACleanRegisterRaisesNothing(t *testing.T) {
	require.Empty(t, portfolio.OpenFindings(nil, noon))
}
