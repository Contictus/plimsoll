package portfolio

import (
	"fmt"
	"sort"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/shopspring/decimal"
)

// This file marks a portfolio to market. It is pure: one run in, one set of numbers and the
// reasons they are qualified by out (L4). Nothing here reads a price -- the run was produced
// once, by the worker, and every number in one response comes from that one run (K11, L10).

// FeeAsset is an asset this account has paid a fee in, as the id the fee resolved to at
// ingest rather than the venue string it arrived as (L8). It is an input to Build because
// whether a fee can be expressed in USD is a property of the run, not of the fee.
type FeeAsset struct {
	ID     int64
	Symbol string
}

// PriceRun is the run a response was built from, carried so a reader can ask the same
// question twice and know whether the answer moved because the market did or because the
// prices did.
type PriceRun struct {
	RunID            int64
	AsOf             time.Time
	Numeraire        string
	Source           string
	AssumedPeg       bool
	OldestObservedAt time.Time
}

// value marks the holdings and balances and returns the reasons the result is qualified by.
// It mutates the portfolio in place because the alternative -- rebuilding both slices --
// would risk the two copies diverging in ordering, and the ordering is what makes two
// responses comparable.
func value(out *Portfolio, in Input) []freshness.Reason {
	if in.Valuation == nil {
		// Narrowed to exactly this in M4: not "no price engine exists" but "no run has
		// completed". A fresh install whose feed has never connected would otherwise serve a
		// confident zero, which is the failure this reason was written to prevent.
		return []freshness.Reason{ValuationUnavailable(in.AsOf)}
	}
	run := in.Valuation

	out.AsOf = run.AsOf
	out.Prices = &PriceRun{
		RunID: run.ID, AsOf: run.AsOf, Numeraire: run.Numeraire, Source: run.PriceSource,
		AssumedPeg: run.AssumedPeg, OldestObservedAt: run.OldestObservedAt,
	}

	total := decimal.Zero
	unpriced := map[string]bool{}
	valued := 0

	for i := range out.Balances {
		b := &out.Balances[i]
		price, ok := priceOf(run, b.AssetID)
		if !ok {
			// A quantity of nothing needs no price: reporting it would be noise, and noise
			// in freshness erodes it exactly as fast as silence does.
			if !b.Quantity.IsZero() {
				unpriced[b.Asset] = true
			}
			continue
		}
		b.PriceUSD = decimal.NullDecimal{Decimal: price, Valid: true}
		b.ValueUSD = money(b.Quantity.Mul(price))
		total = total.Add(b.ValueUSD.Decimal)
		valued++
	}

	for i := range out.Holdings {
		h := &out.Holdings[i]
		base, baseOK := priceOf(run, h.BaseAssetID)
		quote, quoteOK := priceOf(run, h.QuoteAssetID)
		if !baseOK {
			if !h.Quantity.IsZero() {
				unpriced[h.BaseAsset] = true
			}
			continue
		}
		// Signed on purpose: a short's market value is negative because it is exposure owed
		// rather than money held, and flattening it to an absolute value would let a hedged
		// book report twice the risk it carries.
		h.MarketValue = money(h.Quantity.Mul(base))
		if quoteOK {
			// Marked against entry in one step: quantity times the mark, less quantity times
			// what it cost, both converted at this same run. Converting the two legs at two
			// sources is the "every screen shows a different total" failure in miniature.
			h.UnrealizedPnL = money(
				h.Quantity.Mul(base).Sub(h.Quantity.Mul(h.AvgEntryPrice).Mul(quote)))
		}
	}

	// The total is the sum of the balances and never of the positions. A position and the
	// balance its fills moved are two views of one trade; adding them counts the money twice.
	//
	// A run that priced none of what this account holds produces no total at all, rather than
	// the zero the empty sum would be. Excluding part of a portfolio and saying so is honest;
	// excluding all of it and reporting the remainder as a number is the confident-and-wrong
	// answer, and it is indistinguishable on screen from an empty account (L11).
	out.TotalValueUSD = decimal.NullDecimal{
		Decimal: total.Round(moneyScale),
		Valid:   valued > 0 || len(unpriced) == 0,
	}

	out.Unpriced = make([]string, 0, len(unpriced))
	for asset := range unpriced {
		out.Unpriced = append(out.Unpriced, asset)
	}
	sort.Strings(out.Unpriced)

	return qualify(out, in, run)
}

// qualify names everything about this run that a reader would need to know before trusting
// the total (L11).
func qualify(out *Portfolio, in Input, run *valuation.Run) []freshness.Reason {
	var reasons []freshness.Reason

	if len(out.Unpriced) > 0 {
		reasons = append(reasons, freshness.Reason{
			Code:     freshness.ReasonUnknownSymbol,
			Severity: freshness.SeverityError,
			Detail: fmt.Sprintf("no route to %s for %v: the total is what could be priced and"+
				" excludes these", run.Numeraire, out.Unpriced),
			Since: run.AsOf,
		})
	}
	if run.AssumedPeg {
		reasons = append(reasons, AssumedPeg(run.AsOf))
	}
	if stale, age := staleBy(in, run); stale {
		reasons = append(reasons, PriceStale(run.OldestObservedAt, age))
	}
	for _, f := range in.FeeAssets {
		if _, ok := priceOf(run, f.ID); ok {
			continue
		}
		reasons = append(reasons, FeePriceMissing(f.Symbol, run.AsOf))
	}
	return reasons
}

// staleBy reports whether the run's worst leg is older than tolerance, measured against the
// read time rather than the run's own as_of: a run produced an hour ago from prices that
// were fresh when it ran is stale now, and it is now that the reader is asking.
func staleBy(in Input, run *valuation.Run) (bool, time.Duration) {
	if in.PriceTTL <= 0 || run.OldestObservedAt.IsZero() {
		return false, 0
	}
	age := in.AsOf.Sub(run.OldestObservedAt)
	return age > in.PriceTTL, age
}

func priceOf(run *valuation.Run, assetID int64) (decimal.Decimal, bool) {
	p, ok := run.Prices[assetID]
	if !ok || !p.USD.IsPositive() {
		return decimal.Zero, false
	}
	return p.USD, true
}

// money rounds to the scale Postgres would store, so what this engine computes and what a
// NUMERIC(38,18) column holds are the same number (L1).
func money(d decimal.Decimal) decimal.NullDecimal {
	return decimal.NullDecimal{Decimal: d.Round(moneyScale), Valid: true}
}
