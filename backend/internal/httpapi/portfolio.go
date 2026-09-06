package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

// Every money field below is a string, declared as one rather than converted at the edge by
// a marshaller (L1). It makes the OpenAPI document say `type: string`, so a generated client
// cannot produce a float64 parser -- which is the failure this rule exists to prevent, and
// it happens in someone else's codebase where we would never see it.

type feeBody struct {
	Asset  string `json:"asset"`
	Amount string `json:"amount"`
}

type positionBody struct {
	// ID is <integration_id>.<instrument_id>, and it survives a rebuild (K42).
	ID            string    `json:"id"`
	IntegrationID uuid.UUID `json:"integration_id"`
	InstrumentID  int64     `json:"instrument_id"`
	Symbol        string    `json:"symbol"`
	Kind          string    `json:"kind"          enum:"spot,perp"`
	BaseAsset     string    `json:"base_asset"`
	QuoteAsset    string    `json:"quote_asset"`

	Quantity      string `json:"quantity"        doc:"signed: positive long, negative short"`
	AvgEntryPrice string `json:"avg_entry_price" doc:"unsigned, in the quote asset"`
	CostBasis     string `json:"cost_basis"      doc:"unsigned quantity times entry price, in the quote asset"`
	RealizedPnL   string `json:"realized_pnl"    doc:"in the quote asset"`

	// Fees stay per asset and unconverted: a fee in BNB against a USDT position needs a
	// price, and no price source has run (K18, L9).
	Fees []feeBody `json:"fees"`

	Flat          bool      `json:"flat"            doc:"quantity is zero; kept for its realized PnL and fees"`
	LastEventTime time.Time `json:"last_event_time" doc:"the event time of the last fill folded into this row"`
}

type quoteTotalBody struct {
	Asset       string `json:"asset"`
	CostBasis   string `json:"cost_basis"`
	RealizedPnL string `json:"realized_pnl"`
}

// portfolioBody has no total, and will not until a valuation run backs one (K11, L10).
// Realized PnL on BTC-USDT is denominated in USDT and on ETH-BTC in BTC; a field adding
// them would hold a number with no unit. The subtotals say what can honestly be said, and
// freshness carries valuation_unavailable to say why there is nothing more.
type portfolioBody struct {
	freshness.Envelope
	Positions []positionBody   `json:"positions"`
	Subtotals []quoteTotalBody `json:"subtotals_by_quote_asset"`
	Fees      []feeBody        `json:"fees" doc:"every fee this account has paid, per asset, unconverted"`
}

type positionsBody struct {
	freshness.Envelope
	Positions []positionBody `json:"positions"`
}

// onePositionBody nests the position rather than flattening it, so a client parses the same
// object here as in the list. It is also the shape Huma can describe: an embedded unexported
// struct is not promoted into the generated schema, and a body whose OpenAPI document
// disagrees with what it serves is worse than an inconvenient shape.
type onePositionBody struct {
	freshness.Envelope
	Position positionBody `json:"position"`
}

func renderFees(fees []position.FeeTotal) []feeBody {
	out := make([]feeBody, 0, len(fees))
	for _, f := range fees {
		out = append(out, feeBody{Asset: f.Asset, Amount: f.Amount.String()})
	}
	return out
}

func renderPosition(h portfolio.Holding) positionBody {
	return positionBody{
		ID:            h.ID(),
		IntegrationID: h.IntegrationID,
		InstrumentID:  h.InstrumentID,
		Symbol:        h.Symbol,
		Kind:          h.Kind,
		BaseAsset:     h.BaseAsset,
		QuoteAsset:    h.QuoteAsset,
		Quantity:      h.Quantity.String(),
		AvgEntryPrice: h.AvgEntryPrice.String(),
		CostBasis:     h.CostBasis.String(),
		RealizedPnL:   h.RealizedPnL.String(),
		Fees:          renderFees(h.Fees),
		Flat:          h.Flat,
		LastEventTime: h.LastEventTime,
	}
}

func renderPositions(hs []portfolio.Holding) []positionBody {
	out := make([]positionBody, 0, len(hs))
	for _, h := range hs {
		out = append(out, renderPosition(h))
	}
	return out
}

func envelopeOf(p portfolio.Portfolio) freshness.Envelope {
	return freshness.Envelope{AsOf: p.AsOf, Freshness: p.Freshness}
}

// load reads the caller's portfolio. Every endpoint in this file goes through it, so one
// position, the list, and the whole portfolio are built from one read and cannot disagree
// about the same number (L10).
func (d Deps) load(ctx context.Context) (portfolio.Portfolio, error) {
	accountID, ok := AccountFromContext(ctx)
	if !ok {
		return portfolio.Portfolio{}, huma.Error401Unauthorized("unauthorized")
	}
	return portfolio.Load(ctx, d.DB, accountID, d.Now(), d.LeaseTTL)
}

type positionInput struct {
	ID string `path:"id" doc:"<integration_id>.<instrument_id>"`
}

// registerPortfolio wires the read endpoints. None of them writes anything: no ledger event,
// no projection row, no order (L13, ARCHITECTURE.md section 10).
func (d Deps) registerPortfolio(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-portfolio",
		Method:      http.MethodGet,
		Path:        "/portfolio",
		Summary:     "Holdings, subtotals per quote asset, and what the numbers are missing",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body portfolioBody }, error) {
		p, err := d.load(ctx)
		if err != nil {
			return nil, err
		}

		subtotals := make([]quoteTotalBody, 0, len(p.ByQuote))
		for _, q := range p.ByQuote {
			subtotals = append(subtotals, quoteTotalBody{
				Asset:       q.Asset,
				CostBasis:   q.CostBasis.String(),
				RealizedPnL: q.RealizedPnL.String(),
			})
		}
		return &struct{ Body portfolioBody }{Body: portfolioBody{
			Envelope:  envelopeOf(p),
			Positions: renderPositions(p.Holdings),
			Subtotals: subtotals,
			Fees:      renderFees(p.Fees),
		}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-positions",
		Method:      http.MethodGet,
		Path:        "/positions",
		Summary:     "Every position this account holds, flat ones included",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body positionsBody }, error) {
		p, err := d.load(ctx)
		if err != nil {
			return nil, err
		}
		return &struct{ Body positionsBody }{Body: positionsBody{
			Envelope:  envelopeOf(p),
			Positions: renderPositions(p.Holdings),
		}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-position",
		Method:      http.MethodGet,
		Path:        "/positions/{id}",
		Summary:     "One position",
	}, func(ctx context.Context, in *positionInput) (*struct{ Body onePositionBody }, error) {
		// Parsed before anything is read, so a malformed id is a 400 rather than a query.
		if _, _, err := portfolio.ParsePositionID(in.ID); err != nil {
			return nil, huma.Error400BadRequest("malformed position id")
		}
		p, err := d.load(ctx)
		if err != nil {
			return nil, err
		}
		h, found := p.Find(in.ID)
		if !found {
			// 404 and not 403, deliberately. Telling a caller that an id exists but is not
			// theirs is telling them something about an account they cannot see.
			return nil, huma.Error404NotFound("no such position")
		}
		return &struct{ Body onePositionBody }{Body: onePositionBody{
			Envelope: envelopeOf(p),
			Position: renderPosition(h),
		}}, nil
	})
}
