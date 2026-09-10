package alert

import (
	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/shopspring/decimal"
)

// MetricsOf turns one exposure read into the numbers rules are evaluated against.
//
// A metric that could not be computed is ABSENT from the map rather than present as zero.
// That absence is what makes a rule report `unavailable` instead of quietly deciding the
// account is safe -- the same rule as K50's margin buffer, applied one layer up (L11).
func MetricsOf(e portfolio.Exposure) map[Key]decimal.Decimal {
	out := map[Key]decimal.Decimal{}
	portfolioScope := Scope{Kind: ScopePortfolio}

	if e.Equity.Valid {
		out[Key{Metric: MetricGrossExposure, Scope: portfolioScope}] = e.Report.Portfolio.GrossExposure
		if e.Report.Portfolio.Leverage.Valid {
			out[Key{Metric: MetricLeverage, Scope: portfolioScope}] = e.Report.Portfolio.Leverage.Decimal
		}
		if e.Report.Portfolio.NetLeverage.Valid {
			out[Key{Metric: MetricNetLeverage, Scope: portfolioScope}] = e.Report.Portfolio.NetLeverage.Decimal
		}
		for _, c := range e.Report.Portfolio.Concentration {
			// The largest share, because "no asset is more than 40% of the book" is the
			// rule a human writes, and it is one rule rather than one per asset.
			key := Key{Metric: MetricConcentration, Scope: portfolioScope}
			if current, ok := out[key]; !ok || c.Share.GreaterThan(current) {
				out[key] = c.Share
			}
		}
	}

	for _, s := range e.Report.ByStrategy {
		if s.Strategy == "" {
			// The ungrouped bucket is not a strategy anyone can write a rule about: it is
			// whatever is left over, and its membership changes every time the user tags
			// something.
			continue
		}
		scope := Scope{Kind: ScopeStrategy, Name: s.Strategy}
		if !e.Equity.Valid {
			continue
		}
		out[Key{Metric: MetricGrossExposure, Scope: scope}] = s.Metrics.GrossExposure
		if s.Metrics.Leverage.Valid {
			out[Key{Metric: MetricLeverage, Scope: scope}] = s.Metrics.Leverage.Decimal
		}
		if s.Metrics.NetLeverage.Valid {
			out[Key{Metric: MetricNetLeverage, Scope: scope}] = s.Metrics.NetLeverage.Decimal
		}
	}

	// The margin buffer is the sum across integrations, and it is ABSENT unless every
	// integration that should have a capture has one. A sum missing an integration is not a
	// smaller buffer, it is an unknown one -- and a rule watching for it to fall would read
	// the gap as the very danger it is watching for (K50).
	if len(e.Collateral) > 0 {
		buffer := decimal.Zero
		for _, c := range e.Collateral {
			buffer = buffer.Add(collateral.Buffer(collateral.Snapshot{
				MarginBalance:     c.MarginBalance,
				MaintenanceMargin: c.MaintenanceMargin,
			}))
		}
		out[Key{Metric: MetricMarginBuffer, Scope: portfolioScope}] = buffer
	}
	return out
}

// PositionMetricsOf adds the per-position numbers: the distance to each liquidation price,
// and at portfolio scope the NEAREST of them.
//
// Nearest rather than an average, because the account is liquidated one position at a time
// and an average would be dragged safe by every position that is fine.
func PositionMetricsOf(
	positions []portfolio.RiskPosition, into map[Key]decimal.Decimal,
) map[Key]decimal.Decimal {
	if into == nil {
		into = map[Key]decimal.Decimal{}
	}
	nearest := decimal.NullDecimal{}
	for _, p := range positions {
		if !p.Distance.Valid {
			// No liquidation price means no distance -- absent, never "far away" (K6).
			continue
		}
		into[Key{Metric: MetricLiquidationDistance,
			Scope: Scope{Kind: ScopePosition, Name: p.Symbol}}] = p.Distance.Decimal
		if !nearest.Valid || p.Distance.Decimal.LessThan(nearest.Decimal) {
			nearest = p.Distance
		}
	}
	if nearest.Valid {
		into[Key{Metric: MetricLiquidationDistance, Scope: Scope{Kind: ScopePortfolio}}] = nearest.Decimal
	}
	return into
}
