// Package reconciliation asks the one question the rest of the system cannot ask itself: does
// our state match the exchange's?
//
// Everything else in Plimsoll computes from our own ledger, so everything else is internally
// consistent by construction -- including when it is wrong. This package is where an outside
// answer enters, and the comparison and the classification are pure functions of the two sides
// (L4). The I/O lives in the runner.
//
// V1 policy is detect and report, never auto-correct (K55).
package reconciliation

import "github.com/shopspring/decimal"

// SubjectKind says what a delta is about, because a balance and a position are keyed by
// different things and must never share a namespace.
type SubjectKind string

const (
	// SubjectBalance is keyed by asset code.
	SubjectBalance SubjectKind = "balance"

	// SubjectPosition is keyed by instrument id -- never by exchange symbol (L8). A symbol
	// recycled after a delisting would otherwise attach one instrument's quantity to another's,
	// which is the industry's most common silent corruption.
	SubjectPosition SubjectKind = "position"
)

// Ours is our fold: what the ledger says, after the projection has run.
type Ours struct {
	Balances  map[string]decimal.Decimal
	Positions map[int64]decimal.Decimal
}

// Theirs is the venue's answer, decoded from a REST snapshot.
type Theirs struct {
	Balances  map[string]decimal.Decimal
	Positions map[int64]decimal.Decimal
}

// Tolerance is per metric and per asset, never one global epsilon (K54).
//
// One number cannot serve both a quantity in BTC and a quantity in SHIB: pick a threshold
// small enough for BTC and every SHIB rounding is a finding; pick one large enough for SHIB
// and a real BTC position is invisible.
type Tolerance struct {
	// Dust is the per-asset threshold below which a difference is not worth reporting.
	Dust map[string]decimal.Decimal

	// DefaultDust applies to an asset with no entry in Dust.
	DefaultDust decimal.Decimal
}

func (t Tolerance) dustFor(asset string) decimal.Decimal {
	if d, ok := t.Dust[asset]; ok {
		return d
	}
	return t.DefaultDust
}

// Delta is one disagreement, before it has been classified. It carries the magnitude and the
// subject and deliberately nothing else: what KIND of problem it is takes evidence the
// comparison does not have (K54).
type Delta struct {
	Kind    SubjectKind
	Subject string

	// InstrumentID is set for SubjectPosition and zero otherwise.
	InstrumentID int64

	// Delta is ours minus theirs. Negative means we are short of what the venue reports.
	Delta decimal.Decimal
}
