// Package quality is the data-quality register: everything we know to be wrong about our own
// numbers, and everything we cannot account for.
//
// It owns the vocabulary and the record. Two producers write into it -- the coherence checks
// in this package, which need no exchange call, and internal/reconciliation, which compares
// our fold against the venue -- and one endpoint reads it. A finding has a LIFETIME rather
// than a timestamp (K53): the same problem seen on twelve consecutive runs is one finding
// that is still open, not twelve records.
package quality

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Kind is what sort of problem a finding is. The set is closed and matches the schema's CHECK:
// a kind nothing can produce is a row nothing can explain.
type Kind = string

// The reconciliation classification (K54). The first three are decided from evidence; the
// fourth is the honest residual.
const (
	// KindRounding -- the delta is smaller than one step of the subject's own precision. A
	// representation difference, not a missing fact.
	KindRounding Kind = "rounding"

	// KindUnsupported -- our ledger holds a record for this subject that we deliberately do
	// not normalize, large enough to explain the delta. A withdrawal is the common case (F5).
	KindUnsupported Kind = "unsupported"

	// KindDuplicate -- two events with the same venue identity under different integrations.
	// The one way L5's dedup key can still let a trade in twice.
	KindDuplicate Kind = "duplicate"

	// KindMissingEvent -- outside tolerance and nothing above explains it. The residual, and
	// the class that means "we cannot account for this", which is what the user needs to hear.
	KindMissingEvent Kind = "missing_event"

	// KindSnapshotFailed -- we could not ask the exchange at all. Deliberately a finding
	// rather than a skipped run: "we could not check" and "we checked and it was fine" are
	// different claims, and serving the second while the first is true is the failure L11
	// exists to prevent.
	KindSnapshotFailed Kind = "snapshot_failed"
)

// The internal-coherence checks (K14), which need no exchange call.
const (
	// KindNegativeBalance -- the ledger implies selling more than was ever held, so an event
	// is missing. The strongest signal in the system, and it is found without knowing which
	// event is missing.
	KindNegativeBalance Kind = "negative_balance"

	// KindUnresolvedAsset -- an asset or symbol we cannot map. Reported rather than valued at
	// zero, because valuing it at zero makes a total quietly wrong instead of loudly
	// incomplete.
	KindUnresolvedAsset Kind = "unresolved_asset"

	// KindFeePriceMissing -- no price for a fee asset at the event's own time, so the fee
	// cannot be expressed in the numeraire (L9, K23).
	KindFeePriceMissing Kind = "fee_price_missing"

	// KindClockSkew -- our clock and the venue's disagree beyond tolerance, which makes every
	// timestamp-ordered fold suspect (L7).
	KindClockSkew Kind = "clock_skew"
)

// ReconciliationKinds and CoherenceKinds are what each producer is RESPONSIBLE for.
//
// A pass closes the open findings it owns and did not see this time. Without that scope, a
// quiet reconciliation run would close a negative balance nobody has fixed -- the register
// would report that a problem went away because a different process looked somewhere else.
var (
	ReconciliationKinds = []Kind{
		KindRounding, KindUnsupported, KindDuplicate, KindMissingEvent, KindSnapshotFailed,
	}
	CoherenceKinds = []Kind{
		KindNegativeBalance, KindUnresolvedAsset, KindFeePriceMissing, KindClockSkew,
	}
)

// Severity ranks a finding the same way freshness ranks a reason, and for the same purpose:
// a caller reading nothing but the worst severity is still safe.
const (
	SeverityInfo  = "info"
	SeverityWarn  = "warn"
	SeverityError = "error"
)

// Finding is one problem, as a producer sees it. It carries no identity and no timestamps:
// those belong to the record, and a producer that invented them could open the same problem
// twice.
type Finding struct {
	Kind     Kind
	Subject  string
	Severity string
	Detail   string

	// Delta is the size of the disagreement in the subject's own units, and is absent -- not
	// zero -- where the finding has no magnitude. Zero would claim we measured agreement.
	Delta decimal.NullDecimal

	// Raw is the payload the finding was decided from (L15). When a classification turns out
	// wrong in three months, this is the only thing that can settle the argument.
	Raw json.RawMessage
}

// Pass is one producer's complete answer for one integration at one instant.
//
// Owns is the point: it is the set of kinds this producer is responsible for, and therefore
// the set the close sweep is allowed to touch. Found is everything it saw. A pass that saw
// nothing is not an empty pass -- it is the statement that everything it owns is now fine.
type Pass struct {
	At    time.Time
	Owns  []Kind
	Found []Finding
}

// Stored is a finding as the register holds it: the producer's claim plus the lifetime the
// register gave it.
type Stored struct {
	ID            uuid.UUID
	IntegrationID uuid.UUID
	Kind          Kind
	Subject       string
	Severity      string
	Detail        string
	Delta         decimal.NullDecimal
	Raw           json.RawMessage

	// OpenedAt is when the problem STARTED, and does not move while it persists. LastSeenAt
	// moves on every pass that still sees it; Occurrences counts those passes.
	OpenedAt    time.Time
	LastSeenAt  time.Time
	ClosedAt    ClosedAt
	Occurrences int32
}

// ClosedAt is a nullable instant. An open finding has no closing time, and the zero time would
// read as "closed in year one" to anything that forgot to check.
type ClosedAt struct {
	Time  time.Time
	Valid bool
}
