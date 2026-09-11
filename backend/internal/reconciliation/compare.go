package reconciliation

import (
	"sort"

	"github.com/shopspring/decimal"
)

// Compare reports every disagreement outside tolerance between our fold and the venue's
// snapshot. Pure (L4): the two sides and the tolerance are all it reads.
//
// It iterates the UNION of both sides' keys, not ours. An asset we never ingested at all is
// the single most likely thing to be missing, and iterating our own keys alone is exactly how
// it stays invisible.
func Compare(ours Ours, theirs Theirs, tol Tolerance) []Delta {
	var out []Delta

	for _, asset := range unionStrings(ours.Balances, theirs.Balances) {
		delta := ours.Balances[asset].Sub(theirs.Balances[asset])
		if delta.Abs().LessThan(tol.dustFor(asset)) {
			continue
		}
		out = append(out, Delta{Kind: SubjectBalance, Subject: asset, Delta: delta})
	}

	for _, id := range unionInts(ours.Positions, theirs.Positions) {
		delta := ours.Positions[id].Sub(theirs.Positions[id])
		if delta.IsZero() {
			continue
		}
		out = append(out, Delta{
			Kind:         SubjectPosition,
			Subject:      subjectFor(id),
			InstrumentID: id,
			Delta:        delta,
		})
	}

	return out
}

// unionStrings and unionInts give Compare a stable, complete key set. A missing key reads as
// zero, which is correct: an asset the venue omits and one it reports as zero are the same
// claim, and the documentation does not settle which it does (F21).
func unionStrings(a, b map[string]decimal.Decimal) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func unionInts(a, b map[int64]decimal.Decimal) []int64 {
	seen := make(map[int64]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]int64, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
