// Package alert decides when to say something.
//
// The evaluator is pure (L4): rules, the metrics as they currently stand, the state from last
// time and `now` go in; firings and the next state come out. No clock, no database, no
// delivery -- which is what makes hysteresis and cooldown testable without sleeping, and what
// makes the property that matters checkable at all: a metric hovering on a threshold produces
// one alert rather than fifty.
package alert

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// The metrics a rule can watch. A closed set, because a rule naming a metric nothing computes
// is a rule that never fires, and silence is the one failure this package cannot report.
const (
	MetricLeverage            = "leverage"
	MetricNetLeverage         = "net_leverage"
	MetricGrossExposure       = "gross_exposure"
	MetricMarginBuffer        = "margin_buffer"
	MetricLiquidationDistance = "liquidation_distance"
	MetricConcentration       = "concentration"
)

// What a rule is measured over.
const (
	ScopePortfolio = "portfolio"
	ScopeStrategy  = "strategy"
	ScopePosition  = "position"
)

// Comparator is which side of the threshold is the bad side.
type Comparator string

// The two directions a threshold can be crossed.
const (
	Above Comparator = "above"
	Below Comparator = "below"
)

// Kind is what happened to a rule at one evaluation.
type Kind string

// The three outcomes an evaluation can report.
const (
	Fired    Kind = "fired"
	Resolved Kind = "resolved"

	// Unavailable is neither a firing nor silence: the rule was evaluated and the number it
	// watches did not exist. Unknown is not "below the threshold" (L11).
	Unavailable Kind = "unavailable"
)

// Scope names the book a rule watches: the whole portfolio, one strategy, or one position.
type Scope struct {
	Kind string
	Name string
}

// Key identifies one measured number.
type Key struct {
	Metric string
	Scope  Scope
}

// Rule is one threshold the user set.
type Rule struct {
	ID         uuid.UUID
	Metric     string
	Scope      Scope
	Comparator Comparator

	// Trigger and Clear are two different numbers on purpose. One threshold approached from
	// both sides is what makes a metric sitting on it fire at every evaluation; Clear is the
	// far side of a band the metric has to leave before the condition is over.
	Trigger decimal.Decimal
	Clear   decimal.Decimal

	// Cooldown is the shortest gap between two firings of this rule. It suppresses a
	// re-fire; it never suppresses a first one.
	Cooldown time.Duration

	Enabled bool
}

// State is what the evaluator remembers about one rule between runs.
type State struct {
	RuleID      uuid.UUID
	Firing      bool
	Since       time.Time
	LastFiredAt time.Time
}

// Firing is one thing worth saying.
type Firing struct {
	RuleID uuid.UUID
	Kind   Kind
	Metric string
	Scope  Scope

	// Value is invalid for Unavailable, and never rendered as zero: a metric nobody could
	// compute and a metric that is zero are opposite claims.
	Value decimal.NullDecimal
	At    time.Time
}

// Evaluate decides what each rule has to say about the numbers as they now stand, and returns
// the state the next evaluation needs.
//
// The returned state is a new map: the caller's is never mutated, so an evaluation that is
// thrown away -- a transaction that rolls back, a delivery that fails before anything is
// recorded -- leaves nothing behind claiming the alert already fired.
func Evaluate(
	rules []Rule, values map[Key]decimal.Decimal, state map[uuid.UUID]State, now time.Time,
) ([]Firing, map[uuid.UUID]State) {
	next := make(map[uuid.UUID]State, len(state))
	for id, s := range state {
		next[id] = s
	}
	out := make([]Firing, 0)

	for _, r := range rules {
		if !r.Enabled {
			// Not deleted, because the thresholds a user tuned are worth keeping; not
			// evaluated, because that is what the switch means.
			continue
		}

		current := next[r.ID]
		current.RuleID = r.ID

		value, ok := values[Key{Metric: r.Metric, Scope: r.Scope}]
		if !ok {
			// Reported rather than skipped. A rule watching a number that stopped being
			// computed is silent for a reason the user must be able to tell from safety.
			out = append(out, Firing{
				RuleID: r.ID, Kind: Unavailable, Metric: r.Metric, Scope: r.Scope, At: now,
			})
			next[r.ID] = current
			continue
		}

		switch {
		case !current.Firing && breached(r, value):
			// A cooldown that has not expired suppresses this. LastFiredAt is zero for a
			// rule that has never fired, and now.Sub(zero) is two thousand years, so a
			// first fire is never suppressed -- a silence indistinguishable from safety.
			//
			// The IsZero check is therefore redundant, and deliberately kept: it is the
			// only place the intent is written down, and a mutation that removes it is
			// equivalent only for as long as the zero time stays far in the past.
			if r.Cooldown > 0 && !current.LastFiredAt.IsZero() &&
				now.Sub(current.LastFiredAt) < r.Cooldown {
				break
			}
			current.Firing = true
			current.Since = now
			current.LastFiredAt = now
			out = append(out, firing(r, Fired, value, now))

		case current.Firing && cleared(r, value):
			current.Firing = false
			current.Since = now
			out = append(out, firing(r, Resolved, value, now))
		}
		next[r.ID] = current
	}
	return out, next
}

// breached and cleared are deliberately not each other's negation. Between Trigger and Clear
// the metric is in the band: neither newly bad nor recovered, which is the whole of what
// hysteresis is.
func breached(r Rule, value decimal.Decimal) bool {
	if r.Comparator == Below {
		return value.LessThan(r.Trigger)
	}
	return value.GreaterThan(r.Trigger)
}

func cleared(r Rule, value decimal.Decimal) bool {
	if r.Comparator == Below {
		return value.GreaterThan(r.Clear)
	}
	return value.LessThan(r.Clear)
}

func firing(r Rule, kind Kind, value decimal.Decimal, now time.Time) Firing {
	return Firing{
		RuleID: r.ID, Kind: kind, Metric: r.Metric, Scope: r.Scope,
		Value: decimal.NewNullDecimal(value), At: now,
	}
}
