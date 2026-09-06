package ingest_test

import (
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/httpapi"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/stretchr/testify/require"
)

// THE TEST THAT KEEPS A NEW STATE FROM SHIPPING SILENT.
//
// It walks the enum rather than listing the states it happens to know about, so a state
// added later without a freshness entry fails here instead of appearing in production as a
// response that looks fully current while the worker is anything but (L11). Same shape as
// M0's default-deny route test: the failure is the default.
func TestEveryStateDeclaresWhatItMeansForFreshness(t *testing.T) {
	require.NotEmpty(t, ingest.AllStates)
	for _, state := range ingest.AllStates {
		t.Run(string(state), func(t *testing.T) {
			reason, degraded := state.Reason()
			if !degraded {
				require.Equal(t, ingest.StateLive, state,
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
	for _, state := range ingest.AllStates {
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
		in   ingest.Conditions
		want ingest.State
	}{
		{"before the first subscribe", ingest.Conditions{}, ingest.StateConnecting},
		{"subscribed and current", ingest.Conditions{
			Subscribed: true, Connected: true, HistoryComplete: true}, ingest.StateLive},
		{"history still loading", ingest.Conditions{
			Subscribed: true, Connected: true}, ingest.StateBackfilling},
		{"replaying a gap", ingest.Conditions{
			Subscribed: true, Connected: true, HistoryComplete: true, Resyncing: true},
			ingest.StateResyncing},
		{"feed is down", ingest.Conditions{
			Subscribed: true, HistoryComplete: true}, ingest.StateDegraded},
		{"down beats backfilling", ingest.Conditions{Subscribed: true}, ingest.StateDegraded},
		{"down beats resyncing", ingest.Conditions{
			Subscribed: true, HistoryComplete: true, Resyncing: true}, ingest.StateDegraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ingest.Classify(tc.in))
		})
	}
}
