package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/danielgtaylor/huma/v2"
)

// Every money field here is a string for the same reason as everywhere else (L1), and every
// one of them names the instant it belongs to: an interval's numbers come from two ends, and
// a field that did not say which end would be a number with no time attached.

type realizedBody struct {
	Asset       string `json:"asset"`
	RealizedPnL string `json:"realized_pnl"`
}

// pnlBody carries two valuation runs, which L10 permits and in fact requires here: the law
// forbids one number built from two price sources, and no figure below mixes them. A
// question about an interval cannot be answered from one instant's prices.
type pnlBody struct {
	freshness.Envelope

	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	// Realized needs no price at all and is exact. Per quote asset and never summed:
	// realized PnL on BTC-USDT is USDT and on ETH-BTC is BTC (K11).
	Realized []realizedBody `json:"realized_by_quote_asset"`

	UnrealizedAtFrom string `json:"unrealized_usd_at_from" doc:"empty when that end had no run"`
	UnrealizedAtTo   string `json:"unrealized_usd_at_to"`
	TotalAtFrom      string `json:"total_value_usd_at_from"`
	TotalAtTo        string `json:"total_value_usd_at_to"`

	ValuationAtFrom *valuationBody `json:"valuation_at_from"`
	ValuationAtTo   *valuationBody `json:"valuation_at_to"`
}

type pnlInput struct {
	From string `query:"from" doc:"RFC3339 instant, exclusive of what happened before it"`
	To   string `query:"to"   doc:"RFC3339 instant; omit for now"`
}

func (d Deps) registerPnL(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-pnl",
		Method:      http.MethodGet,
		Path:        "/pnl",
		Summary:     "What was closed out between two instants, and what the open positions did",
		Description: "Realized comes from the ledger and needs no prices. Unrealized is the" +
			" open book marked at each end, from a run rebuilt out of price_ticks for that" +
			" instant -- never reaching past a gap to a later price.",
	}, func(ctx context.Context, in *pnlInput) (*struct{ Body pnlBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		w := d.window()

		from, err := time.Parse(time.RFC3339, in.From)
		if err != nil {
			return nil, huma.Error400BadRequest("from must be an RFC3339 instant")
		}
		to := w.Now
		if in.To != "" {
			if to, err = time.Parse(time.RFC3339, in.To); err != nil {
				return nil, huma.Error400BadRequest("to must be an RFC3339 instant")
			}
		}
		if to.After(w.Now) {
			return nil, huma.Error400BadRequest("to is in the future")
		}
		if !to.After(from) {
			return nil, huma.Error400BadRequest("from must be before to")
		}

		out, err := portfolio.LoadPnL(ctx, d.DB, accountID, from.UTC(), to.UTC(), w, d.PegAssets)
		if err != nil {
			return nil, err
		}

		realized := make([]realizedBody, 0, len(out.Realized))
		for _, r := range out.Realized {
			realized = append(realized, realizedBody{
				Asset: r.Asset, RealizedPnL: r.RealizedPnL.String(),
			})
		}
		return &struct{ Body pnlBody }{Body: pnlBody{
			Envelope:         freshness.Envelope{AsOf: out.AsOf, Freshness: out.Freshness},
			From:             out.From,
			To:               out.To,
			Realized:         realized,
			UnrealizedAtFrom: nullText(out.UnrealizedAtFrom),
			UnrealizedAtTo:   nullText(out.UnrealizedAtTo),
			TotalAtFrom:      nullText(out.TotalAtFrom),
			TotalAtTo:        nullText(out.TotalAtTo),
			ValuationAtFrom:  renderValuation(out.AtFrom),
			ValuationAtTo:    renderValuation(out.AtTo),
		}}, nil
	})
}
