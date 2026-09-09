package valuation_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// Asset ids. Named so a failing path reads as assets rather than as integers.
const (
	btc  int64 = 1
	eth  int64 = 2
	usdt int64 = 3
	usdc int64 = 4
	doge int64 = 5
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

var (
	earlier = at("2026-09-09T13:00:00Z")
	later   = at("2026-09-09T14:00:00Z")
)

func pair(id, base, quote int64, price string, observed time.Time) valuation.Pair {
	return valuation.Pair{
		InstrumentID: id, BaseAssetID: base, QuoteAssetID: quote,
		Price: decimal.RequireFromString(price), ObservedAt: observed,
	}
}

// pegUSDC assumes only USDC. USDT is then priced through a real market rather than assumed,
// which is what K17 asks for: a stablecoin with a market is priced, not pegged.
func pegUSDC() valuation.PegSet {
	return valuation.PegSet{usdc: decimal.RequireFromString("1")}
}

func TestADirectPairPricesInOneMarketHop(t *testing.T) {
	rates := valuation.NewRates([]valuation.Pair{
		pair(10, btc, usdc, "78000", later),
	})

	got, err := valuation.PriceOf(btc, rates, pegUSDC())
	require.NoError(t, err)

	require.Equal(t, "78000", got.USD.String())
	require.Len(t, got.Path, 2, "one market hop plus the peg that terminates it")
	require.Equal(t, btc, got.Path[0].From)
	require.Equal(t, usdc, got.Path[0].To)
	require.Equal(t, int64(10), got.Path[0].InstrumentID)
	require.False(t, got.Path[0].Inverted)
	require.True(t, got.Path[1].Assumed, "the last hop is the assumption")
	require.Equal(t, later, got.OldestObservedAt)
}

// The path is the feature. GET /positions/{id}/lineage already answers "which events
// produced this position"; with the path stored it answers "which prices produced this
// number, from which source, observed when".
func TestATwoHopPathMultipliesTheLegsAndRecordsBoth(t *testing.T) {
	rates := valuation.NewRates([]valuation.Pair{
		pair(10, btc, usdt, "78000", later),
		pair(11, usdc, usdt, "0.9995", earlier),
	})

	got, err := valuation.PriceOf(btc, rates, pegUSDC())
	require.NoError(t, err)

	// 78000 USDT per BTC, and 1 USDC costs 0.9995 USDT, so a BTC is 78000/0.9995 USDC.
	require.Equal(t, "78039.019509754877418", got.USD.String())
	require.Len(t, got.Path, 3, "BTC->USDT, USDT->USDC, USDC->USD")
	require.Equal(t, []int64{btc, usdt, usdc}, []int64{
		got.Path[0].From, got.Path[1].From, got.Path[2].From,
	})

	require.Equal(t, earlier, got.OldestObservedAt,
		"the age of a path is the age of its worst leg, never the average")
}

// If only BTCUSDT exists, pricing USDT in BTC divides rather than multiplies. A sign error
// here is a total wrong by orders of magnitude while looking entirely plausible.
func TestAnInvertedPairDividesAndSaysSo(t *testing.T) {
	rates := valuation.NewRates([]valuation.Pair{
		pair(10, usdc, doge, "10", later), // 1 USDC buys 10 DOGE
	})

	got, err := valuation.PriceOf(doge, rates, pegUSDC())
	require.NoError(t, err)

	require.Equal(t, "0.1", got.USD.String(), "a DOGE is a tenth of a USDC")
	require.True(t, got.Path[0].Inverted, "the pair was used in the direction it is not quoted in")
	require.Equal(t, doge, got.Path[0].From)
	require.Equal(t, usdc, got.Path[0].To)
}

// K17, the reason the peg set is configuration rather than a constant: a stablecoin with a
// real market must be priced through it. Assuming USDT is 1.00 when USDCUSDT is trading is
// a blind spot shaped exactly like the product's worst tail risk.
func TestAStablecoinWithAMarketIsPricedRatherThanPegged(t *testing.T) {
	// USDT has depegged to 0.97 USDC. Only USDC is assumed.
	rates := valuation.NewRates([]valuation.Pair{
		pair(11, usdc, usdt, "1.0309278350515464", later), // 1 USDC = 1.0309 USDT
	})

	got, err := valuation.PriceOf(usdt, rates, pegUSDC())
	require.NoError(t, err)

	require.Equal(t, "0.969999999999999992", got.USD.String(),
		"the depeg must show up in the number, not be assumed away")
	require.Len(t, got.Path, 2)
	require.False(t, got.Path[0].Assumed, "the USDT leg came from a market")
}

// An asset that IS the assumption costs one hop and nothing else, and it must be flagged:
// holding USDC means the whole value rests on the assumption rather than on a market.
func TestAPegAssetPricesAsTheAssumptionItself(t *testing.T) {
	got, err := valuation.PriceOf(usdc, valuation.NewRates(nil), pegUSDC())
	require.NoError(t, err)

	require.Equal(t, "1", got.USD.String())
	require.True(t, got.AssumedPeg)
	require.Len(t, got.Path, 1)
	require.True(t, got.Path[0].Assumed)
	require.True(t, got.OldestObservedAt.IsZero(),
		"no market was consulted, so there is no observation to age")
}

// Zero is a number a dashboard will happily add up. An asset with no route must reach the
// caller as a refusal, so the total can say what it is missing instead of being wrong by it.
func TestAnAssetWithNoRouteIsAnErrorAndNotZero(t *testing.T) {
	rates := valuation.NewRates([]valuation.Pair{pair(10, btc, usdc, "78000", later)})

	_, err := valuation.PriceOf(eth, rates, pegUSDC())
	require.ErrorIs(t, err, valuation.ErrNoRoute)
	require.Contains(t, err.Error(), "2", "the error must name the asset that could not be priced")
}

// A price of zero cannot be inverted, and a pair quoted at zero is not a market. Refusing
// is the only honest answer: dividing would panic or produce infinity, and skipping the
// edge silently would route around a broken feed without saying so.
func TestAPairQuotedAtZeroIsNotAUsableEdge(t *testing.T) {
	rates := valuation.NewRates([]valuation.Pair{
		pair(10, usdc, doge, "0", later),
	})

	_, err := valuation.PriceOf(doge, rates, pegUSDC())
	require.ErrorIs(t, err, valuation.ErrNoRoute)
}

// Two runs a second apart must pick the same route, or the total moves for a reason no user
// can see and no lineage can explain. Map iteration order is the classic way to lose this.
func TestThePathIsStableAcrossRuns(t *testing.T) {
	pairs := []valuation.Pair{
		pair(10, btc, usdt, "78000", later),
		pair(11, btc, usdc, "77990", later),
		pair(12, usdc, usdt, "1.0001", later),
		pair(13, btc, eth, "25", later),
		pair(14, eth, usdc, "3120", later),
	}

	first, err := valuation.PriceOf(btc, valuation.NewRates(pairs), pegUSDC())
	require.NoError(t, err)

	for i := 0; i < 50; i++ {
		again, err := valuation.PriceOf(btc, valuation.NewRates(pairs), pegUSDC())
		require.NoError(t, err)
		require.Equal(t, first.USD.String(), again.USD.String(), "run %d", i)
		require.Equal(t, first.Path, again.Path, "run %d picked a different route", i)
	}

	require.Len(t, first.Path, 2, "the fewest-hops route is BTC->USDC->USD")
}

// THE PROPERTY THAT MAKES THE PATH AN AUDIT TRAIL RATHER THAN A DECORATION.
//
// Multiplying the stored rates must reproduce the stored total, exactly. That is only true
// because each leg is rounded to the storage scale as the edge is built, rather than the
// division being carried at higher precision and rounded once at the end -- the second is
// marginally more accurate and cannot be checked by anyone reading the row.
//
// A reader who does not trust the number can do the arithmetic themselves. That is the
// whole product claim, applied to the half M3 could not reach.
func TestThePathMultipliesOutToTheReportedPrice(t *testing.T) {
	rates := valuation.NewRates([]valuation.Pair{
		pair(10, btc, usdt, "78000", later),
		pair(11, usdc, usdt, "0.9995", earlier),
	})

	got, err := valuation.PriceOf(btc, rates, pegUSDC())
	require.NoError(t, err)

	product := decimal.NewFromInt(1)
	for _, hop := range got.Path {
		product = product.Mul(hop.Rate)
	}
	require.Equal(t, got.USD.String(), product.Round(18).String(),
		"the recorded path does not reproduce the recorded price")
}

// The stability test above passes even with the ordering removed, because only one peg is
// reachable in it -- so it proves less than it looks like it proves. This is the case the
// ordering actually exists for: two assumed assets the same distance away, where nothing
// but a deterministic tie-break decides which one terminates the path.
//
// Left to map iteration order, the same portfolio would price through USDC on one read and
// through the other on the next, and the total would move by the spread between two
// stablecoins for a reason no lineage could explain.
func TestTwoPegsAtEqualDistanceAreBrokenDeterministically(t *testing.T) {
	const busd int64 = 6
	pegs := valuation.PegSet{
		usdc: decimal.RequireFromString("1"),
		busd: decimal.RequireFromString("1"),
	}
	pairs := []valuation.Pair{
		pair(11, btc, usdc, "78000", later),
		pair(12, btc, busd, "77950", later),
	}

	first, err := valuation.PriceOf(btc, valuation.NewRates(pairs), pegs)
	require.NoError(t, err)
	require.Equal(t, usdc, first.Path[0].To,
		"the lower asset id must win, and it must win every time")

	for i := 0; i < 100; i++ {
		again, err := valuation.PriceOf(btc, valuation.NewRates(pairs), pegs)
		require.NoError(t, err)
		require.Equal(t, first.Path, again.Path, "run %d took a different route", i)
		require.Equal(t, first.USD.String(), again.USD.String(), "run %d", i)
	}
}

// Two instruments quoting the same two assets -- the same shape as the peg tie above, one
// level down. The lower instrument id wins, so a venue listing a duplicate pair cannot make
// the total flicker.
func TestTwoInstrumentsForOnePairAreBrokenDeterministically(t *testing.T) {
	pairs := []valuation.Pair{
		pair(21, btc, usdc, "78000", later),
		pair(20, btc, usdc, "77990", later),
	}

	for i := 0; i < 100; i++ {
		got, err := valuation.PriceOf(btc, valuation.NewRates(pairs), pegUSDC())
		require.NoError(t, err)
		require.Equal(t, int64(20), got.Path[0].InstrumentID, "run %d", i)
	}
}
