package worker_test

import (
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/httpapi"
	"github.com/Contictus/plimsoll/backend/internal/worker"
	"github.com/stretchr/testify/require"
)

// THE TEST THAT KEEPS A NEW STATE FROM SHIPPING SILENT.
//
// It walks the enum rather than listing the states it happens to know about, so a state
// added later without a freshness entry fails here instead of appearing in production as a
// response that looks fully current while the worker is anything but (L11). Same shape as
// M0's default-deny route test: the failure is the default.
func TestEveryStateDeclaresWhatItMeansForFreshness(t *testing.T) {
	require.NotEmpty(t, worker.AllStates)
	for _, state := range worker.AllStates {
		t.Run(string(state), func(t *testing.T) {
			reason, degraded := state.Reason()
			if !degraded {
				require.Equal(t, worker.StateLive, state,
					"only live may contribute no reason; %s must say what it costs a reader", state)
				return
			}
			require.NotEmpty(t, reason.Code, "%s has no reason code", state)
			require.Contains(t,
				[]httpapi.Severity{httpapi.SeverityInfo, httpapi.SeverityWarn, httpapi.SeverityError},
				reason.Severity, "%s has no severity", state)
			require.NotEmpty(t, reason.Detail, "%s says nothing a human can read", state)
		})
	}
}

// The codes a state can raise are the closed set, not free strings. A typo here is a
// response no client can match on, which is a degradation that reads as silence.
func TestStateReasonsComeFromTheClosedSet(t *testing.T) {
	closed := map[string]bool{
		httpapi.ReasonWSGap:                  true,
		httpapi.ReasonPriceStale:             true,
		httpapi.ReasonBackfillIncomplete:     true,
		httpapi.ReasonHistoryTruncated:       true,
		httpapi.ReasonAssumedPeg:             true,
		httpapi.ReasonUnknownSymbol:          true,
		httpapi.ReasonReconciliationMismatch: true,
		httpapi.ReasonFeePriceMissing:        true,
	}
	for _, state := range worker.AllStates {
		if reason, degraded := state.Reason(); degraded {
			require.True(t, closed[reason.Code], "%s raises %q, which is not a reason code",
				state, reason.Code)
		}
	}
}

// One state at a time, chosen by the worst thing currently true. A worker that is both
// backfilling and disconnected is disconnected first: the reader needs to know the live
// feed is down more than it needs to know history is still loading.
func TestTheStateIsTheWorstConditionCurrentlyTrue(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   worker.Conditions
		want worker.State
	}{
		{"before the first subscribe", worker.Conditions{}, worker.StateConnecting},
		{"subscribed and current", worker.Conditions{
			Subscribed: true, Connected: true, HistoryComplete: true}, worker.StateLive},
		{"history still loading", worker.Conditions{
			Subscribed: true, Connected: true}, worker.StateBackfilling},
		{"replaying a gap", worker.Conditions{
			Subscribed: true, Connected: true, HistoryComplete: true, Resyncing: true},
			worker.StateResyncing},
		{"feed is down", worker.Conditions{
			Subscribed: true, HistoryComplete: true}, worker.StateDegraded},
		{"down beats backfilling", worker.Conditions{Subscribed: true}, worker.StateDegraded},
		{"down beats resyncing", worker.Conditions{
			Subscribed: true, HistoryComplete: true, Resyncing: true}, worker.StateDegraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, worker.Classify(tc.in))
		})
	}
}
