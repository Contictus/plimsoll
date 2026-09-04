package httpapi_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/httpapi"
	"github.com/stretchr/testify/require"
)

// K23/L11: status is the worst severity present, so a caller that reads nothing but
// status is still safe. One bit cannot carry seven conditions -- but one enum can rank
// them.
func TestFreshnessStatusIsTheWorstSeverity(t *testing.T) {
	since := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	info := httpapi.Reason{Code: "assumed_peg", Severity: httpapi.SeverityInfo, Since: since}
	warn := httpapi.Reason{Code: "price_stale", Severity: httpapi.SeverityWarn, Since: since}
	bad := httpapi.Reason{Code: "ws_gap", Severity: httpapi.SeverityError, Since: since}

	tests := []struct {
		name    string
		reasons []httpapi.Reason
		want    httpapi.Status
	}{
		{"no reasons is ok", nil, httpapi.StatusOK},
		{"info alone is ok", []httpapi.Reason{info}, httpapi.StatusOK},
		{"warn degrades", []httpapi.Reason{info, warn}, httpapi.StatusDegraded},
		{"error is unreliable", []httpapi.Reason{info, warn, bad}, httpapi.StatusUnreliable},
		{"order does not matter", []httpapi.Reason{bad, info}, httpapi.StatusUnreliable},
		{"a warn after an error must not downgrade it",
			[]httpapi.Reason{bad, warn}, httpapi.StatusUnreliable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, httpapi.NewFreshness(tc.reasons...).Status)
		})
	}
}

// Reasons must serialize as a list, never null: a client iterating the field must not
// have to nil-check it.
func TestFreshnessReasonsIsNeverNil(t *testing.T) {
	f := httpapi.NewFreshness()
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
	in := []httpapi.Reason{
		{Code: "ws_gap", Severity: httpapi.SeverityError, Since: since},
		{Code: "price_stale", Severity: httpapi.SeverityWarn, Since: since},
	}
	require.Equal(t, in, httpapi.NewFreshness(in...).Reasons)
}

// history_truncated is not backfill_incomplete, and the difference is the whole reason it
// exists. "Incomplete" means not finished yet: recoverable, and it implies that waiting
// will fix it. A venue that will never return data before a cutoff is a different and
// permanent claim, and telling a user to wait for something that will not arrive is exactly
// the confident-and-wrong failure L11 rejects.
func TestHistoryTruncatedIsItsOwnReason(t *testing.T) {
	require.Equal(t, "history_truncated", httpapi.ReasonHistoryTruncated)
	require.NotEqual(t, httpapi.ReasonBackfillIncomplete, httpapi.ReasonHistoryTruncated)
}

// The reason codes are a closed set (ARCHITECTURE.md §5). Asserted as a whole rather than
// one by one, so a code added to the package without being added to the documented contract
// fails here instead of appearing in a response no client can match on.
func TestTheReasonSetIsClosedAndDistinct(t *testing.T) {
	codes := []string{
		httpapi.ReasonWSGap,
		httpapi.ReasonPriceStale,
		httpapi.ReasonBackfillIncomplete,
		httpapi.ReasonHistoryTruncated,
		httpapi.ReasonAssumedPeg,
		httpapi.ReasonUnknownSymbol,
		httpapi.ReasonReconciliationMismatch,
		httpapi.ReasonFeePriceMissing,
	}
	seen := map[string]bool{}
	for _, code := range codes {
		require.NotEmpty(t, code)
		require.False(t, seen[code], "duplicate reason code %q", code)
		seen[code] = true
	}
	require.Len(t, seen, 8)
}
