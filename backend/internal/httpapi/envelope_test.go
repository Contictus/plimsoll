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
