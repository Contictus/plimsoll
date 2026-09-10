// Package risk answers "how exposed is this account, and to what" -- at the portfolio level
// and, which is the point, per strategy (K13).
//
// Pure (L4): positions and their marked values go in, metrics come out. No database, no
// clock, no prices fetched. That is what makes M7.5's scenario shock nearly free -- the same
// function, called with prices that never happened.
package risk

import (
	"sort"

	"github.com/shopspring/decimal"
)

// Position is one position as the read model marked it: signed market value in the run's
// numeraire, the asset it is exposure TO, and the group the user put it in.
type Position struct {
	Symbol    string
	BaseAsset string
	Strategy  string

	// MarketValue is signed -- a short's is negative -- and INVALID when the run could not
	// price it. Invalid is not zero: an unpriced position is named in Report.Unpriced and
	// excluded from every total, because a total that silently absorbed it would report less
	// risk than the account has (L11).
	MarketValue decimal.NullDecimal
}

// AssetExposure is one asset's signed net exposure, in the numeraire.
type AssetExposure struct {
	Asset    string
	Exposure decimal.Decimal
}

// AssetShare is one asset's share of gross exposure.
type AssetShare struct {
	Asset string
	Share decimal.Decimal
}

// Metrics is one book measured: the whole portfolio, or one strategy inside it.
type Metrics struct {
	GrossExposure decimal.Decimal
	NetExposure   decimal.Decimal

	// Leverage is gross over equity and NetLeverage is |net| over equity. Both, because the
	// difference between them IS the hedge: a basis trade has a gross leverage of 2x and a
	// directional leverage of zero, and reporting only the first is what generates the false
	// alert K13 exists to prevent.
	//
	// Invalid rather than infinite when equity is zero. An account with no equity and an open
	// position is in a state a ratio cannot describe.
	Leverage    decimal.NullDecimal
	NetLeverage decimal.NullDecimal

	NetDelta      []AssetExposure
	Concentration []AssetShare
}

// StrategyMetrics is one group's numbers. Strategy is empty for the ungrouped bucket.
type StrategyMetrics struct {
	Strategy string
	Metrics  Metrics
}

// Input is everything one risk report is computed from.
type Input struct {
	// Equity is the account's own, from the valuation run: what it holds, plus the open
	// perpetual PnL the balances do not carry yet. Passed in rather than derived here,
	// because deriving it would mean this package knew about balances, runs and wallets --
	// and then it would need a database.
	Equity    decimal.Decimal
	Positions []Position
}

// Report is the answer, portfolio-wide and per strategy.
type Report struct {
	Portfolio  Metrics
	ByStrategy []StrategyMetrics

	// Unpriced names the assets whose positions could not be valued, so a reader knows what
	// every number above leaves out.
	Unpriced []string
}

// Compute measures the portfolio and every strategy inside it.
//
// Positions the run could not price are excluded from every total and named in Unpriced.
// Untagged positions are their own bucket rather than being dropped: an account halfway
// through tagging its book must still see the whole of it.
func Compute(in Input) Report {
	out := Report{Unpriced: make([]string, 0)}

	books := map[string][]Position{}
	order := []string{}
	priced := make([]Position, 0, len(in.Positions))
	seenUnpriced := map[string]bool{}

	for _, p := range in.Positions {
		if !p.MarketValue.Valid {
			if !seenUnpriced[p.BaseAsset] {
				seenUnpriced[p.BaseAsset] = true
				out.Unpriced = append(out.Unpriced, p.BaseAsset)
			}
			continue
		}
		priced = append(priced, p)
		if _, ok := books[p.Strategy]; !ok {
			order = append(order, p.Strategy)
		}
		books[p.Strategy] = append(books[p.Strategy], p)
	}
	sort.Strings(out.Unpriced)
	sort.Strings(order)

	out.Portfolio = measure(priced, in.Equity)
	out.ByStrategy = make([]StrategyMetrics, 0, len(order))
	for _, name := range order {
		out.ByStrategy = append(out.ByStrategy, StrategyMetrics{
			Strategy: name,
			// Against the ACCOUNT'S equity, not a share of it. Equity is not divisible
			// between strategies -- the margin backing a perp short is the same collateral
			// backing everything else -- so a per-strategy equity would be an invention, and
			// every ratio built on it would inherit the invention.
			Metrics: measure(books[name], in.Equity),
		})
	}
	return out
}

// measure is the arithmetic, applied identically to the portfolio and to one strategy. One
// function rather than two, so the two can never drift into computing leverage differently.
func measure(positions []Position, equity decimal.Decimal) Metrics {
	m := Metrics{
		GrossExposure: decimal.Zero,
		NetExposure:   decimal.Zero,
		NetDelta:      make([]AssetExposure, 0),
		Concentration: make([]AssetShare, 0),
	}

	net := map[string]decimal.Decimal{}
	gross := map[string]decimal.Decimal{}
	assets := []string{}
	for _, p := range positions {
		v := p.MarketValue.Decimal
		if _, ok := net[p.BaseAsset]; !ok {
			assets = append(assets, p.BaseAsset)
			net[p.BaseAsset] = decimal.Zero
			gross[p.BaseAsset] = decimal.Zero
		}
		net[p.BaseAsset] = net[p.BaseAsset].Add(v)
		gross[p.BaseAsset] = gross[p.BaseAsset].Add(v.Abs())

		m.NetExposure = m.NetExposure.Add(v)
		m.GrossExposure = m.GrossExposure.Add(v.Abs())
	}
	sort.Strings(assets)

	for _, a := range assets {
		m.NetDelta = append(m.NetDelta, AssetExposure{Asset: a, Exposure: net[a]})
		if m.GrossExposure.IsPositive() {
			// A share of GROSS, so a short leg contributes its size: risk has no sign, and a
			// concentration built on net would report a hedged book as concentrated in
			// nothing.
			m.Concentration = append(m.Concentration, AssetShare{
				Asset: a, Share: gross[a].Div(m.GrossExposure),
			})
		}
	}

	if equity.IsPositive() {
		m.Leverage = decimal.NewNullDecimal(m.GrossExposure.Div(equity))
		m.NetLeverage = decimal.NewNullDecimal(m.NetExposure.Abs().Div(equity))
	}
	return m
}
