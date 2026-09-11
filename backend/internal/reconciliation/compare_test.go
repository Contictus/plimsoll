package reconciliation_test

import (
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/reconciliation"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// tol is a tolerance with one asset given its own dust threshold, so every test can show that
// the threshold is per asset rather than global.
func tol() reconciliation.Tolerance {
	return reconciliation.Tolerance{
		Dust:        map[string]decimal.Decimal{"BTC": dec("0.00000001"), "SHIB": dec("1")},
		DefaultDust: dec("0.00000001"),
	}
}

func subjects(ds []reconciliation.Delta) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Subject)
	}
	return out
}

func only(t *testing.T, ds []reconciliation.Delta) reconciliation.Delta {
	t.Helper()
	require.Len(t, ds, 1, "expected exactly one delta, got %v", subjects(ds))
	return ds[0]
}

// Agreement produces nothing at all. The register must stay quiet when we are right, or
// nobody will read it when we are not.
func TestAgreementProducesNothing(t *testing.T) {
	ds := reconciliation.Compare(
		reconciliation.Ours{Balances: map[string]decimal.Decimal{"BTC": dec("1.5")}},
		reconciliation.Theirs{Balances: map[string]decimal.Decimal{"BTC": dec("1.5")}},
		tol())
	require.Empty(t, ds)
}

// A difference below the asset's dust threshold is not a disagreement worth a row.
func TestADifferenceWithinToleranceProducesNothing(t *testing.T) {
	ds := reconciliation.Compare(
		reconciliation.Ours{Balances: map[string]decimal.Decimal{"BTC": dec("1.500000001")}},
		reconciliation.Theirs{Balances: map[string]decimal.Decimal{"BTC": dec("1.5")}},
		tol())
	require.Empty(t, ds)
}

// Tolerance is per asset: a quantity that is dust in SHIB is a real position in BTC. One
// epsilon for both is either constant noise or silent blindness (K54).
func TestToleranceIsPerAssetNotGlobal(t *testing.T) {
	const gap = "0.5"
	ours := reconciliation.Ours{Balances: map[string]decimal.Decimal{
		"BTC": dec("1.5"), "SHIB": dec("1000000.5"),
	}}
	theirs := reconciliation.Theirs{Balances: map[string]decimal.Decimal{
		"BTC": dec("1"), "SHIB": dec("1000000"),
	}}

	ds := reconciliation.Compare(ours, theirs, tol())

	require.Equal(t, []string{"BTC"}, subjects(ds),
		"the same %s gap is a real BTC position and SHIB dust", gap)
}

// An asset the venue reports and we have never heard of is a finding, not an absence.
// Iterating only our own keys is how an entirely missing asset stays invisible -- and an asset
// we never ingested at all is the single most likely thing to be missing.
func TestAnAssetOnlyTheExchangeKnowsIsADelta(t *testing.T) {
	ds := reconciliation.Compare(
		reconciliation.Ours{Balances: map[string]decimal.Decimal{}},
		reconciliation.Theirs{Balances: map[string]decimal.Decimal{"SOL": dec("12")}},
		tol())

	d := only(t, ds)
	require.Equal(t, "SOL", d.Subject)
	require.Equal(t, "-12", d.Delta.String(), "ours minus theirs: we are short by twelve")
}

// ...and symmetrically, an asset only we believe in.
func TestAnAssetOnlyWeKnowIsADelta(t *testing.T) {
	ds := reconciliation.Compare(
		reconciliation.Ours{Balances: map[string]decimal.Decimal{"SOL": dec("12")}},
		reconciliation.Theirs{Balances: map[string]decimal.Decimal{}},
		tol())

	require.Equal(t, "12", only(t, ds).Delta.String())
}

// A zero balance on their side and an absent one are the same claim, so neither may produce a
// finding when we also hold nothing. The venue is documented ambiguously on whether it omits
// zero balances (F21) and this is what makes the answer not matter.
func TestAZeroTheyReportAndAZeroTheyOmitAreTheSame(t *testing.T) {
	reported := reconciliation.Compare(
		reconciliation.Ours{Balances: map[string]decimal.Decimal{"BNB": dec("0")}},
		reconciliation.Theirs{Balances: map[string]decimal.Decimal{"BNB": dec("0")}},
		tol())
	omitted := reconciliation.Compare(
		reconciliation.Ours{Balances: map[string]decimal.Decimal{"BNB": dec("0")}},
		reconciliation.Theirs{Balances: map[string]decimal.Decimal{}},
		tol())

	require.Empty(t, reported)
	require.Empty(t, omitted)
}

// Positions are compared by instrument id, never by exchange symbol (L8). A symbol recycled
// after a delisting would otherwise attach one instrument's quantity to another's.
func TestPositionsAreComparedByInstrumentNotSymbol(t *testing.T) {
	ds := reconciliation.Compare(
		reconciliation.Ours{Positions: map[int64]decimal.Decimal{77: dec("2")}},
		reconciliation.Theirs{Positions: map[int64]decimal.Decimal{77: dec("3")}},
		tol())

	d := only(t, ds)
	require.Equal(t, reconciliation.SubjectPosition, d.Kind)
	require.Equal(t, int64(77), d.InstrumentID)
	require.Equal(t, "-1", d.Delta.String())
}

// L4: pure. The same inputs give the same deltas in the same order, or the register would
// reshuffle itself between two runs that found the same disagreement.
func TestCompareIsPureAndOrdered(t *testing.T) {
	ours := reconciliation.Ours{Balances: map[string]decimal.Decimal{
		"ZEC": dec("1"), "AAVE": dec("2"), "BTC": dec("3"),
	}}
	theirs := reconciliation.Theirs{Balances: map[string]decimal.Decimal{}}

	first := reconciliation.Compare(ours, theirs, tol())
	require.Equal(t, first, reconciliation.Compare(ours, theirs, tol()))
	require.Equal(t, []string{"AAVE", "BTC", "ZEC"}, subjects(first))
}
