package alert_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/alert"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

var noon = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// aboveRule fires when leverage climbs past 3 and clears only once it falls back under 2.5.
// The two numbers being different IS the hysteresis: one number approached from both sides is
// what makes a metric hovering on it produce an alert per evaluation.
func aboveRule(cooldown time.Duration) alert.Rule {
	return alert.Rule{
		ID:         uuid.New(),
		Metric:     alert.MetricLeverage,
		Scope:      alert.Scope{Kind: alert.ScopePortfolio},
		Comparator: alert.Above,
		Trigger:    d("3"),
		Clear:      d("2.5"),
		Cooldown:   cooldown,
		Enabled:    true,
	}
}

func values(v string) map[alert.Key]decimal.Decimal {
	return map[alert.Key]decimal.Decimal{
		{Metric: alert.MetricLeverage, Scope: alert.Scope{Kind: alert.ScopePortfolio}}: d(v),
	}
}

// THE FLAPPING TEST.
//
// A metric that crosses the trigger, falls back a hair, and crosses again -- twenty times --
// produces ONE alert. A user messaged twenty times silences the channel, and after that the
// alerting has negative value: it is worse than never having built it, because the user
// believes something is watching.
func TestAMetricHoveringOnTheThresholdFiresOnce(t *testing.T) {
	rule := aboveRule(0)
	state := map[uuid.UUID]alert.State{}

	fired := 0
	at := noon
	for i := 0; i < 20; i++ {
		// Above the trigger, then back into the band between clear and trigger -- which is
		// exactly where a real metric spends its time once it has moved.
		for _, v := range []string{"3.01", "2.99"} {
			at = at.Add(time.Minute)
			out, next := alert.Evaluate([]alert.Rule{rule}, values(v), state, at)
			state = next
			for _, f := range out {
				if f.Kind == alert.Fired {
					fired++
				}
			}
		}
	}
	require.Equal(t, 1, fired,
		"the rule fired %d times while the metric sat on its threshold", fired)
}

// And it fires again once the condition genuinely ends and returns: hysteresis suppresses
// noise, not news.
func TestARuleFiresAgainAfterItHasProperlyCleared(t *testing.T) {
	rule := aboveRule(0)
	state := map[uuid.UUID]alert.State{}

	kinds := []alert.Kind{}
	for i, v := range []string{"3.5", "2.4", "3.5"} {
		out, next := alert.Evaluate([]alert.Rule{rule}, values(v), state,
			noon.Add(time.Duration(i)*time.Hour))
		state = next
		for _, f := range out {
			kinds = append(kinds, f.Kind)
		}
	}
	require.Equal(t, []alert.Kind{alert.Fired, alert.Resolved, alert.Fired}, kinds)
}

// A rule that has fired and then recovers emits a RESOLUTION. Without one the user is left
// staring at a red row for a condition that ended hours ago, and learns that red means
// nothing in particular.
func TestRecoveryIsReported(t *testing.T) {
	rule := aboveRule(0)
	out, state := alert.Evaluate([]alert.Rule{rule}, values("4"), map[uuid.UUID]alert.State{}, noon)
	require.Len(t, out, 1)
	require.Equal(t, alert.Fired, out[0].Kind)
	require.True(t, state[rule.ID].Firing)

	out, state = alert.Evaluate([]alert.Rule{rule}, values("1"), state, noon.Add(time.Hour))
	require.Len(t, out, 1)
	require.Equal(t, alert.Resolved, out[0].Kind)
	require.Equal(t, "1", out[0].Value.Decimal.String())
	require.False(t, state[rule.ID].Firing)
}

// Cooldown suppresses a RE-fire and never a first one. A cooldown that swallowed a first fire
// would be a silence indistinguishable from safety.
func TestCooldownSuppressesAReFireButNeverAFirstFire(t *testing.T) {
	cooled := aboveRule(time.Hour)
	fresh := aboveRule(time.Hour)

	// The cooled rule fired ten minutes ago and has since recovered.
	state := map[uuid.UUID]alert.State{
		cooled.ID: {RuleID: cooled.ID, Firing: false, LastFiredAt: noon.Add(-10 * time.Minute)},
	}

	out, _ := alert.Evaluate([]alert.Rule{cooled, fresh}, values("4"), state, noon)
	require.Len(t, out, 1, "exactly one of the two rules should have fired")
	require.Equal(t, fresh.ID, out[0].RuleID,
		"the rule that has never fired was silenced by another rule's cooldown")
}

func TestCooldownEndsAndTheRuleCanFireAgain(t *testing.T) {
	rule := aboveRule(time.Hour)
	state := map[uuid.UUID]alert.State{
		rule.ID: {RuleID: rule.ID, Firing: false, LastFiredAt: noon.Add(-90 * time.Minute)},
	}
	out, _ := alert.Evaluate([]alert.Rule{rule}, values("4"), state, noon)
	require.Len(t, out, 1)
	require.Equal(t, alert.Fired, out[0].Kind)
}

// A rule on a metric with no value does NOT fire. Unknown is not "below the threshold" (L11,
// and the same rule as K50's margin buffer): a margin buffer nobody could compute is not a
// safe one, and a rule that stayed silent would report safety it cannot see.
func TestARuleOnAMissingMetricDoesNotFireAndSaysSo(t *testing.T) {
	rule := alert.Rule{
		ID: uuid.New(), Metric: alert.MetricMarginBuffer,
		Scope: alert.Scope{Kind: alert.ScopePortfolio}, Comparator: alert.Below,
		Trigger: d("1000"), Clear: d("1500"), Enabled: true,
	}

	out, state := alert.Evaluate([]alert.Rule{rule},
		map[alert.Key]decimal.Decimal{}, map[uuid.UUID]alert.State{}, noon)

	require.Len(t, out, 1)
	require.Equal(t, alert.Unavailable, out[0].Kind)
	require.False(t, out[0].Value.Valid, "an absent metric was rendered as a number")
	require.False(t, state[rule.ID].Firing)
}

// A disabled rule is evaluated by nobody. Deleting it would lose the thresholds the user
// tuned; leaving it live would defeat the switch.
func TestADisabledRuleNeverFires(t *testing.T) {
	rule := aboveRule(0)
	rule.Enabled = false
	out, _ := alert.Evaluate([]alert.Rule{rule}, values("99"), map[uuid.UUID]alert.State{}, noon)
	require.Empty(t, out)
}

// A "below" rule is the one that matters for a margin buffer, and its hysteresis runs the
// other way: it fires as the number falls and clears only once it has climbed back past a
// HIGHER line.
func TestABelowRuleClearsAboveItsTrigger(t *testing.T) {
	rule := alert.Rule{
		ID: uuid.New(), Metric: alert.MetricMarginBuffer,
		Scope: alert.Scope{Kind: alert.ScopePortfolio}, Comparator: alert.Below,
		Trigger: d("1000"), Clear: d("1500"), Enabled: true,
	}
	key := alert.Key{Metric: alert.MetricMarginBuffer, Scope: alert.Scope{Kind: alert.ScopePortfolio}}
	state := map[uuid.UUID]alert.State{}

	// Asserted step by step rather than as a final sequence. Resolving at 1200 and resolving
	// at 1600 produce the same list of kinds, so a test that only compared the list would
	// pass on a rule with no hysteresis at all -- which is the mutation this test exists for.
	step := func(v string, at time.Duration) []alert.Firing {
		out, next := alert.Evaluate([]alert.Rule{rule},
			map[alert.Key]decimal.Decimal{key: d(v)}, state, noon.Add(at))
		state = next
		return out
	}

	fired := step("900", 0)
	require.Len(t, fired, 1)
	require.Equal(t, alert.Fired, fired[0].Kind)

	require.Empty(t, step("1200", time.Hour),
		"1200 is back above the trigger but below the clear line: it must not resolve there")

	resolved := step("1600", 2*time.Hour)
	require.Len(t, resolved, 1)
	require.Equal(t, alert.Resolved, resolved[0].Kind)
}

// L4: the same inputs with the same `now` produce identical output, twice. The evaluator
// reads no clock of its own, which is what makes every test above possible without sleeping.
func TestEvaluationIsDeterministic(t *testing.T) {
	rule := aboveRule(time.Hour)
	state := map[uuid.UUID]alert.State{}

	first, firstState := alert.Evaluate([]alert.Rule{rule}, values("4"), state, noon)
	second, secondState := alert.Evaluate([]alert.Rule{rule}, values("4"), state, noon)

	require.Equal(t, first, second)
	require.Equal(t, firstState, secondState)
}

// A rule scoped to one strategy reads that strategy's number, not the portfolio's. Alerting
// on a leverage the strategy never had is the whole reason the metrics are computed per
// strategy in the first place (K13).
func TestAStrategyScopedRuleReadsItsOwnStrategy(t *testing.T) {
	rule := aboveRule(0)
	rule.Scope = alert.Scope{Kind: alert.ScopeStrategy, Name: "carry"}

	portfolio := alert.Key{Metric: alert.MetricLeverage, Scope: alert.Scope{Kind: alert.ScopePortfolio}}
	carry := alert.Key{Metric: alert.MetricLeverage, Scope: alert.Scope{Kind: alert.ScopeStrategy, Name: "carry"}}

	out, _ := alert.Evaluate([]alert.Rule{rule}, map[alert.Key]decimal.Decimal{
		portfolio: d("9"),
		carry:     d("1"),
	}, map[uuid.UUID]alert.State{}, noon)

	require.Empty(t, out, "the rule fired on the portfolio's leverage instead of its strategy's")
}
