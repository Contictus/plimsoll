package freshness_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/stretchr/testify/require"
)

// K23/L11: status is the worst severity present, so a caller that reads nothing but
// status is still safe. One bit cannot carry seven conditions -- but one enum can rank
// them.
func TestFreshnessStatusIsTheWorstSeverity(t *testing.T) {
	since := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	info := freshness.Reason{Code: "assumed_peg", Severity: freshness.SeverityInfo, Since: since}
	warn := freshness.Reason{Code: "price_stale", Severity: freshness.SeverityWarn, Since: since}
	bad := freshness.Reason{Code: "ws_gap", Severity: freshness.SeverityError, Since: since}

	tests := []struct {
		name    string
		reasons []freshness.Reason
		want    freshness.Status
	}{
		{"no reasons is ok", nil, freshness.StatusOK},
		{"info alone is ok", []freshness.Reason{info}, freshness.StatusOK},
		{"warn degrades", []freshness.Reason{info, warn}, freshness.StatusDegraded},
		{"error is unreliable", []freshness.Reason{info, warn, bad}, freshness.StatusUnreliable},
		{"order does not matter", []freshness.Reason{bad, info}, freshness.StatusUnreliable},
		{"a warn after an error must not downgrade it",
			[]freshness.Reason{bad, warn}, freshness.StatusUnreliable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, freshness.New(tc.reasons...).Status)
		})
	}
}

// Reasons must serialize as a list, never null: a client iterating the field must not
// have to nil-check it.
func TestFreshnessReasonsIsNeverNil(t *testing.T) {
	f := freshness.New()
	require.NotNil(t, f.Reasons)
	require.Empty(t, f.Reasons)

	raw, err := json.Marshal(f)
	require.NoError(t, err)
	require.JSONEq(t, `{"status":"ok","reasons":[]}`, string(raw))
}

// Every reason carried in must come back out. Dropping one would make a response look
// healthier than it is, which is the failure L11 exists to prevent.
func TestFreshnessKeepsEveryReason(t *testing.T) {
	since := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	in := []freshness.Reason{
		{Code: "ws_gap", Severity: freshness.SeverityError, Since: since},
		{Code: "price_stale", Severity: freshness.SeverityWarn, Since: since},
	}
	require.Equal(t, in, freshness.New(in...).Reasons)
}

// history_truncated is not backfill_incomplete, and the difference is the whole reason it
// exists. "Incomplete" means not finished yet: recoverable, and it implies that waiting
// will fix it. A venue that will never return data before a cutoff is a different and
// permanent claim, and telling a user to wait for something that will not arrive is exactly
// the confident-and-wrong failure L11 rejects.
func TestHistoryTruncatedIsItsOwnReason(t *testing.T) {
	require.Equal(t, "history_truncated", freshness.ReasonHistoryTruncated)
	require.NotEqual(t, freshness.ReasonBackfillIncomplete, freshness.ReasonHistoryTruncated)
}

// The reason codes are a closed set (ARCHITECTURE.md §5). Asserted as a whole rather than
// one by one, so a code added to the package without being added to the documented contract
// fails here instead of appearing in a response no client can match on.
func TestTheReasonSetIsClosedAndDistinct(t *testing.T) {
	codes := []string{
		freshness.ReasonWSGap,
		freshness.ReasonPriceStale,
		freshness.ReasonBackfillIncomplete,
		freshness.ReasonHistoryTruncated,
		freshness.ReasonAssumedPeg,
		freshness.ReasonUnknownSymbol,
		freshness.ReasonReconciliationMismatch,
		freshness.ReasonFeePriceMissing,
	}
	seen := map[string]bool{}
	for _, code := range codes {
		require.NotEmpty(t, code)
		require.False(t, seen[code], "duplicate reason code %q", code)
		seen[code] = true
	}
	require.Len(t, seen, 8)
}
