package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

// Every number here is a string for the same reason as everywhere else (L1). The one that
// most needs saying is liquidation_distance: it is empty rather than "0" when the venue
// reports no liquidation price, because a flat position has none and rendering that as zero
// would put "0% from liquidation" on a screen describing an account holding nothing.

type riskPositionBody struct {
	IntegrationID uuid.UUID `json:"integration_id"`
	InstrumentID  int64     `json:"instrument_id"`
	Symbol        string    `json:"symbol"`

	Quantity         string `json:"quantity"          doc:"signed: positive long, negative short"`
	EntryPrice       string `json:"entry_price"`
	MarkPrice        string `json:"mark_price"`
	LiquidationPrice string `json:"liquidation_price" doc:"the venue's own, never ours (K6)"`
	Notional         string `json:"notional"`
	Leverage         string `json:"leverage"`
	MaintMargin      string `json:"maint_margin"      doc:"from the venue's tier table (F15)"`

	LiquidationDistance string `json:"liquidation_distance" doc:"|mark - liquidation| / mark; empty when there is no liquidation price"`
}

type riskIntegrationBody struct {
	IntegrationID uuid.UUID `json:"integration_id"`
	Exchange      string    `json:"exchange"`
	Label         string    `json:"label"`

	// AsOf is this integration's own capture. The envelope's as_of is the oldest of them,
	// so a client reading only the envelope is never told the screen is fresher than its
	// stalest part.
	AsOf time.Time `json:"as_of"`

	MarginBalance     string `json:"margin_balance"`
	WalletBalance     string `json:"wallet_balance"`
	UnrealizedPnL     string `json:"unrealized_pnl"`
	MaintenanceMargin string `json:"maintenance_margin"`
	AvailableBalance  string `json:"available_balance"`

	// MarginBuffer is signed and never clamped: an account past the line is not "at zero",
	// and how far past is the whole question.
	MarginBuffer string `json:"margin_buffer" doc:"margin balance less the maintenance requirement; negative means past the line"`

	Positions []riskPositionBody `json:"positions"`
}

// riskBody is per integration and never summed across them. A total that quietly omitted an
// integration whose capture failed would be the confident-and-wrong number this endpoint
// exists to prevent, and the omission would be invisible in the one figure people read.
type riskBody struct {
	freshness.Envelope

	Integrations []riskIntegrationBody `json:"integrations"`
}

type fundingRowBody struct {
	IntegrationID uuid.UUID `json:"integration_id"`
	InstrumentID  int64     `json:"instrument_id"`
	Symbol        string    `json:"symbol"`
	Asset         string    `json:"asset" doc:"the asset the payments were denominated in"`

	Total  string `json:"total"  doc:"signed: negative is funding paid, positive is funding received"`
	Events int64  `json:"events" doc:"how many payments this total is the sum of"`

	FirstEventTime time.Time `json:"first_event_time"`
	LastEventTime  time.Time `json:"last_event_time"`
}

type fundingBody struct {
	freshness.Envelope

	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	Funding []fundingRowBody `json:"funding"`
}

type fundingInput struct {
	From string `query:"from" doc:"RFC3339 instant, inclusive"`
	To   string `query:"to"   doc:"RFC3339 instant, exclusive; omit for now"`
}

func (d Deps) registerRisk(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-risk",
		Method:      http.MethodGet,
		Path:        "/risk",
		Summary:     "How close this account is to liquidation",
		Description: "Every number comes from one captured snapshot per integration, named" +
			" in as_of. A capture that has aged is served with collateral_stale; an" +
			" integration with no capture is omitted and raises collateral_unavailable," +
			" because an unknown margin buffer and a zero one are opposite claims.",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body riskBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		out, err := portfolio.LoadRisk(ctx, d.DB, accountID, d.window(), d.CollateralTTL)
		if err != nil {
			return nil, err
		}
		return &struct{ Body riskBody }{Body: riskBody{
			Envelope:     freshness.Envelope{AsOf: out.AsOf, Freshness: out.Freshness},
			Integrations: renderRisk(out.Integrations),
		}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-funding",
		Method:      http.MethodGet,
		Path:        "/funding",
		Summary:     "What funding cost or paid, per instrument, over a window",
		Description: "Summed from the ledger and exact: a funding payment is a cash flow" +
			" the venue already denominated, so no price is involved and the total is the" +
			" events it names, added up.",
	}, func(ctx context.Context, in *fundingInput) (*struct{ Body fundingBody }, error) {
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
		if !to.After(from) {
			return nil, huma.Error400BadRequest("from must be before to")
		}

		out, err := portfolio.LoadFunding(ctx, d.DB, accountID, from.UTC(), to.UTC(), w)
		if err != nil {
			return nil, err
		}

		rows := make([]fundingRowBody, 0, len(out.BySymbol))
		for _, r := range out.BySymbol {
			rows = append(rows, fundingRowBody{
				IntegrationID:  r.IntegrationID,
				InstrumentID:   r.InstrumentID,
				Symbol:         r.Symbol,
				Asset:          r.Asset,
				Total:          r.Total.String(),
				Events:         r.Events,
				FirstEventTime: r.FirstEventTime,
				LastEventTime:  r.LastEventTime,
			})
		}
		return &struct{ Body fundingBody }{Body: fundingBody{
			Envelope: freshness.Envelope{AsOf: out.AsOf, Freshness: out.Freshness},
			From:     out.From,
			To:       out.To,
			Funding:  rows,
		}}, nil
	})
}

func renderRisk(in []portfolio.IntegrationRisk) []riskIntegrationBody {
	out := make([]riskIntegrationBody, 0, len(in))
	for _, r := range in {
		positions := make([]riskPositionBody, 0, len(r.Positions))
		for _, p := range r.Positions {
			positions = append(positions, riskPositionBody{
				IntegrationID:       p.IntegrationID,
				InstrumentID:        p.InstrumentID,
				Symbol:              p.Symbol,
				Quantity:            p.Quantity.String(),
				EntryPrice:          p.EntryPrice.String(),
				MarkPrice:           p.MarkPrice.String(),
				LiquidationPrice:    p.LiquidationPrice.String(),
				Notional:            p.Notional.String(),
				Leverage:            p.Leverage.String(),
				MaintMargin:         p.MaintMargin.String(),
				LiquidationDistance: nullText(p.Distance),
			})
		}
		out = append(out, riskIntegrationBody{
			IntegrationID:     r.IntegrationID,
			Exchange:          r.Exchange,
			Label:             r.Label,
			AsOf:              r.AsOf,
			MarginBalance:     r.MarginBalance.String(),
			WalletBalance:     r.WalletBalance.String(),
			UnrealizedPnL:     r.UnrealizedPnL.String(),
			MaintenanceMargin: r.MaintenanceMargin.String(),
			AvailableBalance:  r.AvailableBalance.String(),
			MarginBuffer:      r.MarginBuffer.String(),
			Positions:         positions,
		})
	}
	return out
}
