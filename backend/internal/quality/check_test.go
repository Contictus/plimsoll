package quality_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func kinds(found []quality.Finding) []string {
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.Kind)
	}
	return out
}

func subjectOf(t *testing.T, found []quality.Finding, kind string) quality.Finding {
	t.Helper()
	for _, f := range found {
		if f.Kind == kind {
			return f
		}
	}
	t.Fatalf("no finding of kind %q in %v", kind, kinds(found))
	return quality.Finding{}
}

// baseline is an account with nothing wrong with it. Every test below changes exactly one
// thing, so a finding that appears is attributable to that change and nothing else.
func baseline() quality.Input {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	return quality.Input{
		Balances:      map[string]decimal.Decimal{"BTC": dec("1.5"), "USDT": dec("2000")},
		Precision:     map[string]int32{"BTC": 8, "USDT": 8},
		Now:           now,
		ExchangeClock: now,
		SkewTolerance: 5 * time.Second,
	}
}

// The strongest signal in the system. The ledger implies selling more than was ever held, so
// an event is missing -- and the check finds it WITHOUT knowing which event is missing. That
// property is the whole point, so this asserts it from a balance alone.
func TestANegativeBalanceIsAMissingEventWithoutKnowingWhichOne(t *testing.T) {
	in := baseline()
	in.Balances["ETH"] = dec("-3.25")

	found := quality.Check(in)

	f := subjectOf(t, found, quality.KindNegativeBalance)
	require.Equal(t, "ETH", f.Subject)
	require.Equal(t, quality.SeverityError, f.Severity,
		"a balance that cannot exist is not a warning")
	require.True(t, f.Delta.Valid)
	require.Equal(t, "-3.25", f.Delta.Decimal.String(),
		"the delta is how much is missing, and it is the only thing we know")
}

// Exactly zero is not negative. The boundary is the entire check: an account that sold
// everything it held is correct, not broken, and reporting it would make the register noise.
func TestAZeroBalanceIsNotAFinding(t *testing.T) {
	in := baseline()
	in.Balances["ETH"] = dec("0")

	require.Empty(t, quality.Check(in), "a flat balance is not a problem")
}

// Dust below the asset's own precision is a rounding artefact, not a missing event. Without
// this, an account carrying a 1e-18 remainder is permanently "unreliable" and the user learns
// to ignore the one surface that is supposed to tell them the truth.
func TestDustBelowPrecisionIsRoundingNotMissingEvent(t *testing.T) {
	in := baseline()
	// BTC has 8 decimal places, so anything smaller than 1e-8 cannot be a real holding.
	in.Balances["BTC"] = dec("-0.000000001")

	found := quality.Check(in)

	require.Equal(t, []string{quality.KindRounding}, kinds(found),
		"sub-precision dust is a representation difference, not a missing event")
	require.Equal(t, quality.SeverityInfo, subjectOf(t, found, quality.KindRounding).Severity)
}

// One step of precision is NOT dust: it is the smallest real amount the asset has. Putting the
// boundary on the wrong side of it hides the smallest genuine discrepancy the venue can report.
func TestExactlyOneStepOfPrecisionIsARealFinding(t *testing.T) {
	in := baseline()
	in.Balances["BTC"] = dec("-0.00000001")

	require.Equal(t, []string{quality.KindNegativeBalance}, kinds(quality.Check(in)))
}

// An asset we cannot resolve is reported as unresolved rather than valued at zero. Valuing it
// at zero makes a total quietly wrong instead of loudly incomplete, which is the exact trade
// L11 refuses.
func TestAnUnresolvableAssetIsAFindingNotAZero(t *testing.T) {
	in := baseline()
	in.Unresolved = []string{"WTF"}

	found := quality.Check(in)

	f := subjectOf(t, found, quality.KindUnresolvedAsset)
	require.Equal(t, "WTF", f.Subject)
	require.False(t, f.Delta.Valid,
		"an asset we cannot resolve has no known magnitude, and zero would claim we measured one")
}

// Our clock against the venue's: beyond tolerance every timestamp-ordered fold is suspect, so
// it is reported before it can corrupt an ordering (L7).
func TestClockSkewBeyondToleranceIsReported(t *testing.T) {
	in := baseline()
	in.ExchangeClock = in.Now.Add(30 * time.Second)

	found := quality.Check(in)
	require.Equal(t, []string{quality.KindClockSkew}, kinds(found))
}

// Skew is symmetric. Being ahead of the venue is exactly as bad as being behind it, and a
// check that only looks one way is blind to half of the condition it was written for.
func TestClockSkewIsReportedInBothDirections(t *testing.T) {
	behind, ahead := baseline(), baseline()
	behind.ExchangeClock = behind.Now.Add(30 * time.Second)
	ahead.ExchangeClock = ahead.Now.Add(-30 * time.Second)

	require.Equal(t, []string{quality.KindClockSkew}, kinds(quality.Check(behind)))
	require.Equal(t, []string{quality.KindClockSkew}, kinds(quality.Check(ahead)))
}

// Skew exactly at tolerance is within tolerance. A tolerance that excludes its own boundary
// makes every tolerance in the system a different kind of number.
func TestSkewExactlyAtToleranceIsAccepted(t *testing.T) {
	in := baseline()
	in.ExchangeClock = in.Now.Add(5 * time.Second)

	require.Empty(t, quality.Check(in))
}

// L4: Check reads no clock and no network. The same input twice gives the same answer, in the
// same order, or the register would reshuffle itself between two runs that found nothing new.
func TestCheckIsPure(t *testing.T) {
	in := baseline()
	in.Balances["ETH"] = dec("-3.25")
	in.Balances["AAA"] = dec("-1")
	in.Precision["AAA"] = 8
	in.Unresolved = []string{"WTF", "ZZZ"}

	first := quality.Check(in)
	second := quality.Check(in)
	require.Equal(t, first, second)
	require.Equal(t, first, quality.Check(in))
	require.Greater(t, len(first), 1, "the ordering claim is vacuous with one finding")
}
