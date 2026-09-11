package reconciliation_test

import (
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/Contictus/plimsoll/backend/internal/reconciliation"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func balanceDelta(subject, delta string) reconciliation.Delta {
	return reconciliation.Delta{
		Kind:    reconciliation.SubjectBalance,
		Subject: subject,
		Delta:   dec(delta),
	}
}

// A delta smaller than one step of the subject's own precision is a representation
// difference, not a missing fact. It is classified first, because a rounding artefact that
// reaches the residual is reported to the user as "we cannot account for this" -- which is
// both wrong and alarming.
func TestASubPrecisionDeltaIsRounding(t *testing.T) {
	got := reconciliation.Classify(
		balanceDelta("BTC", "0.000000001"),
		reconciliation.Evidence{Precision: map[string]int32{"BTC": 8}},
	)
	require.Equal(t, quality.KindRounding, got)
}

// THE CLASSIFIER TEST THAT MATTERS.
//
// We hold more than the venue does, and the ledger contains a withdrawal we deliberately do
// not normalize (F5) large enough to explain it. That is `unsupported`, NOT `duplicate`.
//
// Sign alone decides nothing: "we have more than them" is explained just as well by a
// withdrawal we never ingested as by a double count, and calling it a duplicate sends the user
// hunting for a second copy of a trade that does not exist (K54).
func TestHoldingMoreThanTheExchangeIsNotAutomaticallyADuplicate(t *testing.T) {
	d := balanceDelta("BTC", "0.5")

	got := reconciliation.Classify(d, reconciliation.Evidence{
		Precision:            map[string]int32{"BTC": 8},
		UnsupportedMagnitude: map[string]decimal.Decimal{"BTC": dec("0.5")},
	})

	require.Equal(t, quality.KindUnsupported, got)
}

// The same delta with no evidence at all is the residual: we cannot account for it, and that
// is exactly what the user needs to be told.
func TestAnUnexplainedDeltaIsAMissingEvent(t *testing.T) {
	got := reconciliation.Classify(
		balanceDelta("BTC", "0.5"),
		reconciliation.Evidence{Precision: map[string]int32{"BTC": 8}},
	)
	require.Equal(t, quality.KindMissingEvent, got)
}

// An unsupported record too small to account for the gap explains nothing. Accepting it would
// let a 0.001 withdrawal excuse a 5 BTC hole -- the classification would be technically
// derived from evidence and still completely wrong.
func TestAnUnsupportedRecordTooSmallToExplainTheGapDoesNot(t *testing.T) {
	got := reconciliation.Classify(
		balanceDelta("BTC", "5"),
		reconciliation.Evidence{
			Precision:            map[string]int32{"BTC": 8},
			UnsupportedMagnitude: map[string]decimal.Decimal{"BTC": dec("0.001")},
		},
	)
	require.Equal(t, quality.KindMissingEvent, got)
}

// A true duplicate is decided from the ledger, not from the sign of a number: one venue
// identity appearing under two integrations is the one way L5's dedup key can still let a
// trade in twice, because the key is scoped to the integration.
func TestOneVenueIdentityUnderTwoIntegrationsIsADuplicate(t *testing.T) {
	got := reconciliation.Classify(
		balanceDelta("BTC", "0.5"),
		reconciliation.Evidence{
			Precision:      map[string]int32{"BTC": 8},
			DuplicatedHere: true,
		},
	)
	require.Equal(t, quality.KindDuplicate, got)
}

// Evidence of a duplicate outranks evidence of an unsupported record. A duplicate is a defect
// in our own ingest and is actionable; an unsupported record is a known gap and is not. When
// both are present, reporting the actionable one is what makes the register worth reading.
func TestADuplicateOutranksAnUnsupportedRecord(t *testing.T) {
	got := reconciliation.Classify(
		balanceDelta("BTC", "0.5"),
		reconciliation.Evidence{
			Precision:            map[string]int32{"BTC": 8},
			UnsupportedMagnitude: map[string]decimal.Decimal{"BTC": dec("0.5")},
			DuplicatedHere:       true,
		},
	)
	require.Equal(t, quality.KindDuplicate, got)
}

// A sign flip must not change the classification on its own. This is the property the whole
// design of K54 rests on, so it is asserted directly rather than implied by the cases above.
func TestTheSignAloneNeverDecidesTheClass(t *testing.T) {
	ev := reconciliation.Evidence{Precision: map[string]int32{"BTC": 8}}

	require.Equal(t,
		reconciliation.Classify(balanceDelta("BTC", "0.5"), ev),
		reconciliation.Classify(balanceDelta("BTC", "-0.5"), ev),
		"the same magnitude with opposite signs and identical evidence is the same class")
}
