package collateral_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// The margin buffer is what is left before liquidation starts, and it is signed.
//
// A buffer clamped at zero hides the one state the user most needs to see: an account whose
// maintenance requirement already exceeds its margin balance is not "at zero", it is past
// the line, and the size of how far past is the whole question.
func TestTheMarginBufferIsSignedAndIsNotClamped(t *testing.T) {
	comfortable := collateral.Snapshot{
		MarginBalance:     d("10000"),
		MaintenanceMargin: d("1500"),
	}
	require.Equal(t, "8500", collateral.Buffer(comfortable).String())

	underwater := collateral.Snapshot{
		MarginBalance:     d("1000"),
		MaintenanceMargin: d("1500"),
	}
	require.Equal(t, "-500", collateral.Buffer(underwater).String(),
		"a clamped buffer hides the only state that matters")
}

// Liquidation distance is |mark - liq| / mark, and K6's whole point is that the liquidation
// price is READ rather than computed -- so this function's only job is the ratio.
func TestLiquidationDistanceIsTheRatioToTheMark(t *testing.T) {
	got := collateral.Distance(d("60000"), d("48000"))
	require.True(t, got.Valid)
	require.Equal(t, "0.2", got.Decimal.String())

	// A short liquidates upward, and the distance is a magnitude either way.
	got = collateral.Distance(d("60000"), d("72000"))
	require.True(t, got.Valid)
	require.Equal(t, "0.2", got.Decimal.String())
}

// A flat position has no liquidation price, and the venue reports zero for it. Rendering
// that as a distance of 1.0 -- or as infinity -- teaches a reader to ignore the field on
// exactly the day it says something.
//
// Absent is a different claim from far away, and the type says so.
func TestThereIsNoDistanceWithoutALiquidationPrice(t *testing.T) {
	require.False(t, collateral.Distance(d("60000"), d("0")).Valid)
	require.False(t, collateral.Distance(d("0"), d("48000")).Valid,
		"a mark of zero would divide by zero; there is no distance to report")
}

// brackets is the shape /fapi/v1/leverageBracket returns (F15), smallest notional first.
func brackets() []collateral.Bracket {
	return []collateral.Bracket{
		{NotionalFloor: d("0"), NotionalCap: d("50000"), MaintMarginRatio: d("0.004"), Cum: d("0")},
		{NotionalFloor: d("50000"), NotionalCap: d("250000"), MaintMarginRatio: d("0.005"), Cum: d("50")},
		{NotionalFloor: d("250000"), NotionalCap: d("1000000"), MaintMarginRatio: d("0.01"), Cum: d("1300")},
	}
}

// THE TEST M7.5 DEPENDS ON.
//
// The maintenance requirement is not a flat percentage: it is a tiered table, and a big
// enough position crosses into a higher rate. `notional * rate - cum` is the venue's own
// formula, and `cum` is what makes the tiers continuous instead of a step function that
// would jump the requirement at every boundary.
//
// A scenario shock changes the notional. Scaling today's maintenance margin by the price
// move -- the obvious shortcut -- silently assumes the rate is constant, which is true right
// up until the shock is large enough to matter, and then wrong in the direction that
// understates the danger.
func TestMaintenanceMarginCrossesBrackets(t *testing.T) {
	for _, tc := range []struct{ notional, want, why string }{
		{"10000", "40", "10000 * 0.004 - 0"},
		{"50000", "200", "the boundary belongs to the bracket it opens"},
		{"100000", "450", "100000 * 0.005 - 50"},
		{"500000", "3700", "500000 * 0.01 - 1300"},
	} {
		got, err := collateral.MaintenanceAt(brackets(), d(tc.notional))
		require.NoError(t, err, tc.notional)
		require.Equal(t, tc.want, got.String(), "%s (%s)", tc.notional, tc.why)
	}
}

// A notional above every bracket is an error rather than the last bracket's rate. The table
// ends where the venue's own risk model ends: beyond it the exchange would not have let the
// position be opened, so answering anyway is inventing a number for a position that cannot
// exist -- and inventing it on the low side, which is the dangerous direction.
func TestANotionalAboveTheTableIsRefused(t *testing.T) {
	_, err := collateral.MaintenanceAt(brackets(), d("2000000"))
	require.ErrorIs(t, err, collateral.ErrOutsideBrackets)
}

// A short position's notional is negative in some of the venue's fields and the requirement
// is on the size, not the direction. Taken as an absolute so a short is not handed a
// negative maintenance requirement, which would read as free margin.
func TestMaintenanceMarginIsAboutSizeNotDirection(t *testing.T) {
	long, err := collateral.MaintenanceAt(brackets(), d("100000"))
	require.NoError(t, err)
	short, err := collateral.MaintenanceAt(brackets(), d("-100000"))
	require.NoError(t, err)
	require.Equal(t, long.String(), short.String())
}

// An empty table is not "no requirement". It is a capture that failed, and answering zero
// would report an account with infinite margin buffer at the exact moment we know least
// about it.
func TestAnEmptyBracketTableIsRefused(t *testing.T) {
	_, err := collateral.MaintenanceAt(nil, d("100"))
	require.Error(t, err)
}

// F14: maintenance margin comes from /fapi/v3/account and the liquidation price from
// /fapi/v3/positionRisk. Two calls, two instants -- and a margin buffer from 12:00:00 beside
// a liquidation price from 12:00:30 is one screen describing two different accounts.
//
// The snapshot is only as fresh as its stalest half, so AsOf is the EARLIER of the two, and
// a gap wider than the tolerance is refused rather than averaged.
func TestASnapshotIsOnlyAsFreshAsItsStalestHalf(t *testing.T) {
	account := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	positions := account.Add(3 * time.Second)

	s, err := collateral.NewSnapshot(account, positions, time.Minute)
	require.NoError(t, err)
	require.Equal(t, account, s.AsOf, "the snapshot claimed to be fresher than its older half")

	_, err = collateral.NewSnapshot(account, account.Add(5*time.Minute), time.Minute)
	require.ErrorIs(t, err, collateral.ErrSnapshotTorn)

	// And the other way round: which call happened first is not knowable in advance.
	_, err = collateral.NewSnapshot(account, account.Add(-5*time.Minute), time.Minute)
	require.ErrorIs(t, err, collateral.ErrSnapshotTorn)
}
