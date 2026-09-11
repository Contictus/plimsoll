package reconciliation

import (
	"strconv"

	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/shopspring/decimal"
)

// Evidence is what the classifier is allowed to reason from. Everything here comes from our
// own ledger and registry, not from the shape of the number being classified.
//
// This type existing at all is the point of K54: without evidence the only thing left to
// classify by is the sign of the delta, and the sign decides nothing.
type Evidence struct {
	// Precision is how many decimal places the subject actually has. Anything below one step
	// of it is a representation difference rather than a missing fact.
	Precision map[string]int32

	// UnsupportedMagnitude is how much of this subject moved through records we deliberately
	// do not normalize -- a withdrawal, most often, because NormalizeWithdrawal does not exist
	// by decision rather than by omission (F5).
	UnsupportedMagnitude map[string]decimal.Decimal

	// DuplicatedHere means the ledger holds two events with the same venue identity under
	// different integrations. That is the one way L5's dedup key can still let a trade in
	// twice, because the key is scoped to the integration that saw it.
	DuplicatedHere bool
}

// Classify decides what KIND of problem a delta is, from evidence rather than from its sign.
//
// The obvious mapping -- they have more, so we are missing an event; we have more, so we
// counted one twice -- is wrong. "We have more than them" is explained just as well by a
// withdrawal we never ingested as by a double count, and classifying it as a duplicate sends
// the user hunting for a second copy of a trade that does not exist (K54).
//
// The order below is the ranking, and it is deliberate: a duplicate is a defect in our own
// ingest and is actionable, an unsupported record is a known gap and is not, and rounding is
// neither. What is left is the residual.
func Classify(d Delta, ev Evidence) quality.Kind {
	magnitude := d.Delta.Abs()

	if magnitude.LessThan(stepFor(ev.Precision, d.Subject)) {
		return quality.KindRounding
	}
	if ev.DuplicatedHere {
		return quality.KindDuplicate
	}
	// An unsupported record only explains the gap if it is big enough to. Accepting a smaller
	// one would let a 0.001 withdrawal excuse a 5 BTC hole -- derived from evidence, and
	// completely wrong.
	if explained, ok := ev.UnsupportedMagnitude[d.Subject]; ok &&
		explained.Abs().GreaterThanOrEqual(magnitude) {
		return quality.KindUnsupported
	}

	// The residual. It means "we cannot account for this", which is precisely what the user
	// needs to be told -- and the class that a resync is the answer to (K55).
	return quality.KindMissingEvent
}

// defaultPrecision matches the coherence checks: the widest precision in the registry, so a
// subject whose precision we do not know has its discrepancy reported rather than dismissed.
const defaultPrecision int32 = 18

func stepFor(precision map[string]int32, subject string) decimal.Decimal {
	places, ok := precision[subject]
	if !ok {
		places = defaultPrecision
	}
	return decimal.New(1, -places)
}

// subjectFor labels a position by its instrument id. It is a label a human reads; nothing
// joins on it, because the id travels beside it in the Delta (L8).
func subjectFor(instrumentID int64) string {
	return "instrument:" + strconv.FormatInt(instrumentID, 10)
}
