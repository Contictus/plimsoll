package transfer_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/transfer"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

var noon = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

var (
	binance = uuid.New()
	bybit   = uuid.New()
)

func out(id, asset, amount string, at time.Time, txid string) transfer.Leg {
	return transfer.Leg{
		EventID:       id,
		IntegrationID: binance,
		Venue:         "binance",
		Asset:         asset,
		Quantity:      dec(amount),
		TxID:          txid,
		EventTime:     at,
	}
}

func in(id, asset, amount string, at time.Time, txid string) transfer.Leg {
	return transfer.Leg{
		EventID:       id,
		IntegrationID: bybit,
		Venue:         "bybit",
		Asset:         asset,
		Quantity:      dec(amount),
		TxID:          txid,
		EventTime:     at,
	}
}

func rules() transfer.Rules {
	return transfer.Rules{
		Window:       6 * time.Hour,
		FeeTolerance: dec("0.02"),
	}
}

func pairs(m transfer.Result) [][2]string {
	got := make([][2]string, 0, len(m.Links))
	for _, l := range m.Links {
		got = append(got, [2]string{l.Out.EventID, l.In.EventID})
	}
	return got
}

// The ordinary case: coins leave one venue and arrive at another, minus a network fee.
func TestAWithdrawalAndItsDepositAreOneTransfer(t *testing.T) {
	got := transfer.Match(
		[]transfer.Leg{out("w1", "BTC", "1.0", noon, "")},
		[]transfer.Leg{in("d1", "BTC", "0.999", noon.Add(20*time.Minute), "")},
		rules())

	require.Equal(t, [][2]string{{"w1", "d1"}}, pairs(got))
	require.Empty(t, got.UnmatchedOut)
	require.Empty(t, got.UnmatchedIn)
}

// A txid match is PROOF and outranks a closer amount-and-window candidate.
//
// Two chains agreeing on a transaction hash is not a coincidence; an amount inside a window is
// a strong guess. A guess that overrides proof joins the wrong two legs and reports one real
// transfer twice (K57).
func TestATxidMatchOutranksACloserGuess(t *testing.T) {
	got := transfer.Match(
		[]transfer.Leg{out("w1", "BTC", "1.0", noon, "0xabc")},
		[]transfer.Leg{
			// Nearer in time and nearer in amount, but a different chain transaction.
			in("decoy", "BTC", "1.0", noon.Add(time.Minute), "0xzzz"),
			in("d1", "BTC", "0.99", noon.Add(3*time.Hour), "0xabc"),
		},
		rules())

	require.Equal(t, [][2]string{{"w1", "d1"}}, pairs(got))
	require.Equal(t, []string{"decoy"}, ids(got.UnmatchedIn))
}

// A match consumes both halves. Without that, one deposit is claimed by two withdrawals of the
// same size -- which is exactly what an account moving the same round number twice a week
// produces, and the second link says the money went somewhere it did not (K57).
func TestOneDepositCannotSettleTwoWithdrawals(t *testing.T) {
	got := transfer.Match(
		[]transfer.Leg{
			out("w1", "USDT", "500", noon, ""),
			out("w2", "USDT", "500", noon.Add(time.Hour), ""),
		},
		[]transfer.Leg{in("d1", "USDT", "500", noon.Add(90*time.Minute), "")},
		rules())

	require.Len(t, got.Links, 1)
	require.Len(t, got.UnmatchedOut, 1, "the other withdrawal is unmatched, not double-linked")
}

// Ambiguity is refused rather than broken by time. When two candidates fit equally well,
// neither is linked: picking the nearer one is right most of the time, and the times it is
// wrong are indistinguishable from the times it is right (K57).
func TestTwoEquallyGoodCandidatesAreBothLeftToTheQueue(t *testing.T) {
	got := transfer.Match(
		[]transfer.Leg{out("w1", "USDT", "500", noon, "")},
		[]transfer.Leg{
			in("d1", "USDT", "500", noon.Add(time.Hour), ""),
			in("d2", "USDT", "500", noon.Add(time.Hour), ""),
		},
		rules())

	require.Empty(t, got.Links, "neither candidate may be chosen")
	require.Equal(t, []string{"w1"}, ids(got.UnmatchedOut))
	require.Len(t, got.UnmatchedIn, 2)
}

// A deposit outside the window is not this withdrawal's other half, however well the amount
// fits. Widening the window until something matches is how two unrelated movements are joined.
func TestADepositOutsideTheWindowIsNotAMatch(t *testing.T) {
	got := transfer.Match(
		[]transfer.Leg{out("w1", "BTC", "1.0", noon, "")},
		[]transfer.Leg{in("d1", "BTC", "1.0", noon.Add(7*time.Hour), "")},
		rules())

	require.Empty(t, got.Links)
}

// A deposit BEFORE its withdrawal is not its other half either. Coins do not arrive before
// they leave, and a symmetric window would happily say they did.
func TestADepositThatArrivedBeforeTheWithdrawalIsNotAMatch(t *testing.T) {
	got := transfer.Match(
		[]transfer.Leg{out("w1", "BTC", "1.0", noon, "")},
		[]transfer.Leg{in("d1", "BTC", "1.0", noon.Add(-time.Minute), "")},
		rules())

	require.Empty(t, got.Links)
}

// A different asset is a different movement, whatever the numbers say.
func TestTheAssetMustAgree(t *testing.T) {
	got := transfer.Match(
		[]transfer.Leg{out("w1", "BTC", "1.0", noon, "")},
		[]transfer.Leg{in("d1", "ETH", "1.0", noon.Add(time.Hour), "")},
		rules())

	require.Empty(t, got.Links)
}

// The deposit is smaller than the withdrawal by the network fee, and only by that. A deposit
// LARGER than its withdrawal is not a fee, it is a different transfer.
func TestADepositLargerThanItsWithdrawalIsNotAMatch(t *testing.T) {
	got := transfer.Match(
		[]transfer.Leg{out("w1", "BTC", "1.0", noon, "")},
		[]transfer.Leg{in("d1", "BTC", "1.01", noon.Add(time.Hour), "")},
		rules())

	require.Empty(t, got.Links, "money does not appear in transit")
}

// A shortfall beyond the fee tolerance is not a fee. Accepting it would let a partial
// withdrawal be reported as a completed one.
func TestAShortfallBeyondToleranceIsNotAFee(t *testing.T) {
	got := transfer.Match(
		[]transfer.Leg{out("w1", "BTC", "1.0", noon, "")},
		[]transfer.Leg{in("d1", "BTC", "0.5", noon.Add(time.Hour), "")},
		rules())

	require.Empty(t, got.Links)
}

// Two legs on the SAME integration are not a cross-venue transfer. That case is K49's, it
// reports one row naming both wallets, and matching it here would double-count it.
func TestTwoLegsOnOneIntegrationAreNotACrossVenueTransfer(t *testing.T) {
	same := out("d1", "BTC", "1.0", noon.Add(time.Hour), "")
	same.EventID = "d1"

	got := transfer.Match(
		[]transfer.Leg{out("w1", "BTC", "1.0", noon, "")},
		[]transfer.Leg{same},
		rules())

	require.Empty(t, got.Links)
}

// L4: pure, and stable. Two runs over the same legs produce the same links in the same order,
// or a re-run would rewrite a queue the user is working through.
func TestMatchIsPureAndOrdered(t *testing.T) {
	outs := []transfer.Leg{
		out("w2", "BTC", "1.0", noon.Add(time.Hour), ""),
		out("w1", "BTC", "2.0", noon, ""),
	}
	ins := []transfer.Leg{
		in("d2", "BTC", "1.0", noon.Add(2*time.Hour), ""),
		in("d1", "BTC", "2.0", noon.Add(30*time.Minute), ""),
	}

	first := transfer.Match(outs, ins, rules())
	require.Equal(t, first, transfer.Match(outs, ins, rules()))
	require.Equal(t, [][2]string{{"w1", "d1"}, {"w2", "d2"}}, pairs(first))
}

func ids(legs []transfer.Leg) []string {
	out := make([]string, 0, len(legs))
	for _, l := range legs {
		out = append(out, l.EventID)
	}
	return out
}

// The answer must not depend on the order the rows came back in.
//
// The heuristic is greedy: an earlier withdrawal claims a deposit a later one might also have
// wanted. That makes the ORDER the withdrawals are considered in load-bearing, and the only
// order that is stable across runs is the one the events happened in.
//
// Here w1 can only reach d1, and w2 can reach both. Oldest-first, w1 takes d1 and w2 takes d2:
// two transfers, which is the truth. Newest-first, w2 sees two candidates, refuses as
// ambiguous, and d2 is orphaned -- so a database that returned the same rows in a different
// order would hand the user a different queue.
func TestTheAnswerDoesNotDependOnTheOrderTheRowsArrivedIn(t *testing.T) {
	w1 := out("w1", "BTC", "1.0", noon, "")
	w2 := out("w2", "BTC", "1.0", noon.Add(time.Hour), "")
	d1 := in("d1", "BTC", "1.0", noon.Add(2*time.Hour), "")
	// Inside w2's six-hour window and outside w1's.
	d2 := in("d2", "BTC", "1.0", noon.Add(6*time.Hour+30*time.Minute), "")

	forward := transfer.Match([]transfer.Leg{w1, w2}, []transfer.Leg{d1, d2}, rules())
	reversed := transfer.Match([]transfer.Leg{w2, w1}, []transfer.Leg{d2, d1}, rules())

	require.Equal(t, [][2]string{{"w1", "d1"}, {"w2", "d2"}}, pairs(forward))
	require.Equal(t, forward, reversed,
		"the same events in a different order are the same events")
}

// A batch withdrawal can put two deposits on one chain transaction, so proof can be ambiguous
// too. The earlier deposit is the one taken, and taken only once -- but the point of the test
// is that the choice is decided by the events rather than by the query plan.
func TestTwoDepositsSharingATxidResolveByTimeAndNotByArrival(t *testing.T) {
	w := out("w1", "BTC", "1.0", noon, "0xabc")
	early := in("d-early", "BTC", "1.0", noon.Add(time.Hour), "0xabc")
	late := in("d-late", "BTC", "1.0", noon.Add(2*time.Hour), "0xabc")

	forward := transfer.Match([]transfer.Leg{w}, []transfer.Leg{early, late}, rules())
	reversed := transfer.Match([]transfer.Leg{w}, []transfer.Leg{late, early}, rules())

	require.Equal(t, [][2]string{{"w1", "d-early"}}, pairs(forward))
	require.Equal(t, forward, reversed)
}
