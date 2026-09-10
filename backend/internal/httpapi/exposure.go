package httpapi

import (
	"context"
	"net/http"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/risk"
	"github.com/danielgtaylor/huma/v2"
)

// Every number here is a string (L1), including the ratios: a leverage parsed as a float is
// rounded in exactly the digits a margin call is decided by, and it would be rounded in
// someone else's codebase where we would never see it.

type assetExposureBody struct {
	Asset    string `json:"asset"`
	Exposure string `json:"exposure" doc:"signed, in the run's numeraire"`
}

type assetShareBody struct {
	Asset string `json:"asset"`
	Share string `json:"share" doc:"this asset's part of gross exposure"`
}

// metricsBody is one book measured. Both leverage ratios are here on purpose: their
// difference is the hedge, and a single field would have to pick one (K13).
type metricsBody struct {
	GrossExposure string `json:"gross_exposure" doc:"sum of |notional|; empty when nothing could be priced"`
	NetExposure   string `json:"net_exposure"   doc:"signed sum"`

	Leverage    string `json:"leverage"     doc:"gross over equity; empty when equity is zero or unknown"`
	NetLeverage string `json:"net_leverage" doc:"directional leverage: |net| over equity"`

	NetDelta      []assetExposureBody `json:"net_delta"`
	Concentration []assetShareBody    `json:"concentration"`
}

// The metrics are nested rather than flattened into their parent, here and below, because an
// embedded unexported struct is not promoted into the OpenAPI schema Huma generates -- and a
// body whose document disagrees with what it serves is worse than an extra level of nesting.
// It also means one shape describes the portfolio and a strategy, so a client parses one
// thing twice rather than two things once.
type strategyExposureBody struct {
	Strategy string      `json:"strategy" doc:"empty means the positions the user has not grouped"`
	Metrics  metricsBody `json:"metrics"`
}

type exposureBody struct {
	freshness.Envelope

	Equity    string      `json:"equity" doc:"valued holdings plus open perpetual PnL; empty when no run has completed"`
	Portfolio metricsBody `json:"portfolio"`

	ByStrategy []strategyExposureBody `json:"by_strategy"`

	// UnpricedAssets is what every number above leaves out. Named rather than absorbed as
	// zero: a total that swallowed an unpriceable position reports less risk than the account
	// has (L11).
	UnpricedAssets []string `json:"unpriced_assets"`
}

func (d Deps) registerExposure(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-exposure",
		Method:      http.MethodGet,
		Path:        "/exposure",
		Summary:     "What this account is exposed to, per asset and per strategy",
		Description: "Gross and net leverage are both reported because their difference is" +
			" the hedge: a delta-neutral basis trade has a gross leverage of 2x and a" +
			" directional leverage of zero, and reporting only the first is what generates" +
			" an alert the user learns to ignore (K13).",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body exposureBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		out, err := portfolio.LoadExposure(ctx, d.DB, accountID, d.window(), d.CollateralTTL)
		if err != nil {
			return nil, err
		}

		byStrategy := make([]strategyExposureBody, 0, len(out.Report.ByStrategy))
		for _, s := range out.Report.ByStrategy {
			byStrategy = append(byStrategy, strategyExposureBody{
				Strategy: s.Strategy,
				Metrics:  renderMetrics(s.Metrics, out.Equity.Valid),
			})
		}
		return &struct{ Body exposureBody }{Body: exposureBody{
			Envelope:       freshness.Envelope{AsOf: out.AsOf, Freshness: out.Freshness},
			Equity:         nullText(out.Equity),
			Portfolio:      renderMetrics(out.Report.Portfolio, out.Equity.Valid),
			ByStrategy:     byStrategy,
			UnpricedAssets: out.Report.Unpriced,
		}}, nil
	})
}

// renderMetrics leaves the exposures empty when nothing could be priced at all. An exposure
// of "0" from an account nobody could value is the confident-zero this envelope exists to
// keep out of a response.
func renderMetrics(m risk.Metrics, priced bool) metricsBody {
	out := metricsBody{
		Leverage:      nullText(m.Leverage),
		NetLeverage:   nullText(m.NetLeverage),
		NetDelta:      make([]assetExposureBody, 0, len(m.NetDelta)),
		Concentration: make([]assetShareBody, 0, len(m.Concentration)),
	}
	if priced {
		out.GrossExposure = m.GrossExposure.String()
		out.NetExposure = m.NetExposure.String()
	}
	for _, a := range m.NetDelta {
		out.NetDelta = append(out.NetDelta,
			assetExposureBody{Asset: a.Asset, Exposure: a.Exposure.String()})
	}
	for _, c := range m.Concentration {
		out.Concentration = append(out.Concentration,
			assetShareBody{Asset: c.Asset, Share: c.Share.String()})
	}
	return out
}
