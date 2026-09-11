package quality

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Input is everything the coherence checks need, and nothing more.
//
// There is no database handle, no clock and no network here (L4). `Now` and `ExchangeClock`
// are values because a check that read the clock itself could not be asked what it would have
// said an hour ago -- and "what did this look like before the resync?" is a question the
// register exists to answer.
type Input struct {
	// Balances is the asset fold: what our ledger says the account holds, per asset code.
	Balances map[string]decimal.Decimal

	// Precision is how many decimal places each asset actually has. Anything smaller than one
	// step cannot be a real holding, which is what separates dust from a missing event.
	Precision map[string]int32

	// Unresolved is every asset or symbol we could not map (L8, K10).
	Unresolved []string

	// Now is our clock; ExchangeClock is the venue's own instant, taken from the payload that
	// carried it (F21). The difference between them is the skew.
	Now           time.Time
	ExchangeClock time.Time
	SkewTolerance time.Duration
}

// defaultPrecision is used for an asset whose precision we do not know.
//
// Eighteen places is the widest any asset in the registry can be, so the dust threshold is the
// narrowest possible: an unknown asset's discrepancy is reported rather than dismissed. Erring
// the other way would let a real missing event be filed as dust because we never looked up how
// many decimals the asset has.
const defaultPrecision int32 = 18

// Check reports everything incoherent about our own state. It makes no exchange call: every
// finding here is decidable from the ledger alone, which is why it still runs when the venue
// is unreachable -- the moment its answers matter most.
//
// Pure (L4): same input, same findings, same order.
func Check(in Input) []Finding {
	var out []Finding

	for _, asset := range sortedKeys(in.Balances) {
		held := in.Balances[asset]
		if !held.IsNegative() {
			continue
		}
		// A balance smaller than one step of the asset's own precision cannot be a real
		// holding, so it is a representation difference rather than a missing fact (K54).
		if held.Abs().LessThan(step(in.Precision, asset)) {
			out = append(out, Finding{
				Kind:     KindRounding,
				Subject:  asset,
				Severity: SeverityInfo,
				Detail: fmt.Sprintf(
					"%s is negative by less than one step of its own precision", asset),
				Delta: decimal.NewNullDecimal(held),
			})
			continue
		}
		out = append(out, Finding{
			Kind:     KindNegativeBalance,
			Subject:  asset,
			Severity: SeverityError,
			Detail: fmt.Sprintf(
				"the ledger implies selling more %s than was ever held, so an event is missing",
				asset),
			Delta: decimal.NewNullDecimal(held),
		})
	}

	for _, symbol := range sortedCopy(in.Unresolved) {
		out = append(out, Finding{
			Kind:     KindUnresolvedAsset,
			Subject:  symbol,
			Severity: SeverityWarn,
			Detail:   fmt.Sprintf("%q maps to no asset in the registry at its own event time", symbol),
			// Deliberately absent: we do not know how much of it there is. Zero would claim
			// we measured agreement (L11).
		})
	}

	if skew := in.ExchangeClock.Sub(in.Now); abs(skew) > in.SkewTolerance {
		out = append(out, Finding{
			Kind:     KindClockSkew,
			Subject:  "",
			Severity: SeverityWarn,
			Detail: fmt.Sprintf(
				"the venue's clock is %s from ours, beyond the %s tolerance; event ordering is suspect",
				skew, in.SkewTolerance),
		})
	}

	return out
}

// step is the smallest amount the asset can actually hold: 10^-precision.
func step(precision map[string]int32, asset string) decimal.Decimal {
	places, ok := precision[asset]
	if !ok {
		places = defaultPrecision
	}
	return decimal.New(1, -places)
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// sortedKeys and sortedCopy are what make Check's output stable. Ranging a map directly would
// reorder the register between two runs that found exactly the same thing.
func sortedKeys(m map[string]decimal.Decimal) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
