package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
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

	// MarketValue and UnrealizedPnL are empty rather than "0" when the run could not price
	// this position's assets. A price we do not have and a price of zero are different
	// claims, and collapsing them is how an unpriceable holding becomes a confident nothing.
	MarketValue   string `json:"market_value"   doc:"signed, in the run's numeraire; empty when unpriced"`
	UnrealizedPnL string `json:"unrealized_pnl" doc:"market value less cost, both at this run; empty when unpriced"`

	Flat          bool      `json:"flat"            doc:"quantity is zero; kept for its realized PnL and fees"`
	LastEventTime time.Time `json:"last_event_time" doc:"the event time of the last fill folded into this row"`
}

// balanceBody is how much of one asset is actually held, as opposed to what a position
// cost. For a spot account this is the question being asked, and M3 shipped without it.
type balanceBody struct {
	IntegrationID uuid.UUID `json:"integration_id"`
	Asset         string    `json:"asset"`
	Quantity      string    `json:"quantity"`

	// Negative means the ledger implies holding less than nothing, which cannot be true of
	// an exchange account: an event is missing (K14). Surfaced as a field so a client can
	// mark the row, and as a freshness reason so a client reading only status is still
	// warned.
	Negative      bool      `json:"negative"`
	LastEventTime time.Time `json:"last_event_time"`

	PriceUSD string `json:"price_usd" doc:"this asset's price in the run; empty when unpriced"`
	ValueUSD string `json:"value_usd" doc:"quantity times price; empty when unpriced"`
}

type quoteTotalBody struct {
	Asset       string `json:"asset"`
	CostBasis   string `json:"cost_basis"`
	RealizedPnL string `json:"realized_pnl"`
}

// valuationBody is the one run this response was priced from (K11, L10). It is served, not
// merely used, because "what is my portfolio worth" is only answerable together with "priced
// how, and how old" -- and a client that can see the run can tell a moving market from a
// moving price source.
type valuationBody struct {
	RunID      int64     `json:"run_id"`
	AsOf       time.Time `json:"as_of"`
	Numeraire  string    `json:"numeraire"`
	Source     string    `json:"price_source"`
	AssumedPeg bool      `json:"assumed_peg" doc:"a leg of this run assumed a peg rather than a traded price"`

	// Rebuilt says this run was reconstructed from price_ticks for a past instant rather
	// than recorded when it happened (K48). A reader comparing the two is entitled to know
	// which they have: one is what we said at the time, the other is what the ticks say now.
	Rebuilt          bool       `json:"rebuilt"`
	OldestObservedAt *time.Time `json:"oldest_observed_at" doc:"the age of the run's worst leg"`
}

// portfolioBody carries a total only when a run backs one, and says what the total leaves
// out when it does. The subtotals per quote asset stay: they are exact, they need no price,
// and a reader reconciling against an exchange screen denominated in USDT wants them.
//
// The total is the sum of the valued balances and never of the positions. A position and the
// balance its fills moved are two views of one trade, and adding them counts the money twice.
type portfolioBody struct {
	freshness.Envelope

	// TotalValueUSD is empty when no run has completed, and freshness says valuation_unavailable
	// -- an empty field rather than a zero, because an account nobody could price is not an
	// account worth nothing (L11).
	TotalValueUSD string         `json:"total_value_usd"`
	Valuation     *valuationBody `json:"valuation" doc:"the run every number here was priced from; null when there is none"`

	// UnpricedAssets is what the total excludes. "Worth X" and "worth X, minus the part we
	// could not price" are different sentences, and only one of them is true.
	UnpricedAssets []string `json:"unpriced_assets"`

	Positions []positionBody   `json:"positions"`
	Balances  []balanceBody    `json:"balances"`
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
		MarketValue:   nullText(h.MarketValue),
		UnrealizedPnL: nullText(h.UnrealizedPnL),
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

func renderBalances(bs []portfolio.Balance) []balanceBody {
	out := make([]balanceBody, 0, len(bs))
	for _, b := range bs {
		out = append(out, balanceBody{
			IntegrationID: b.IntegrationID,
			Asset:         b.Asset,
			Quantity:      b.Quantity.String(),
			Negative:      b.Negative,
			LastEventTime: b.LastEventTime,
			PriceUSD:      nullText(b.PriceUSD),
			ValueUSD:      nullText(b.ValueUSD),
		})
	}
	return out
}

// renderValuation is nil when no run backs the response, which is the shape that makes the
// absence unmissable: a client reading total_value_usd without checking this gets an empty
// string, not a zero it could mistake for an answer.
func renderValuation(r *portfolio.PriceRun) *valuationBody {
	if r == nil {
		return nil
	}
	out := &valuationBody{
		RunID: r.RunID, AsOf: r.AsOf, Numeraire: r.Numeraire,
		Source: r.Source, AssumedPeg: r.AssumedPeg,
		Rebuilt: r.RunID == valuation.EphemeralRunID,
	}
	if !r.OldestObservedAt.IsZero() {
		observed := r.OldestObservedAt
		out.OldestObservedAt = &observed
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
	return portfolio.Load(ctx, d.DB, accountID, d.window())
}

// loadAt answers for a past instant by folding the ledger to it and rebuilding a run from
// price_ticks. A future instant is refused rather than served: the honest answer would be
// today's portfolio with tomorrow's timestamp on it, which is a claim we cannot make.
func (d Deps) loadAt(ctx context.Context, at time.Time) (portfolio.Portfolio, error) {
	accountID, ok := AccountFromContext(ctx)
	if !ok {
		return portfolio.Portfolio{}, huma.Error401Unauthorized("unauthorized")
	}
	w := d.window()
	if at.After(w.Now) {
		return portfolio.Portfolio{}, huma.Error400BadRequest("at is in the future")
	}
	return portfolio.LoadAt(ctx, d.DB, accountID, at.UTC(), w, d.PegAssets)
}

// window is the one place the API turns its configuration into the read model's, so two
// endpoints cannot judge the same condition by two tolerances.
func (d Deps) window() portfolio.Window {
	return portfolio.Window{Now: d.Now(), LeaseTTL: d.LeaseTTL, PriceTTL: d.PriceTTL}
}

// portfolioInput carries the optional instant. Absent means now, which is the live
// projection; present means the fold, and the two paths produce the same shape of body so a
// client parses one thing.
type portfolioInput struct {
	At string `query:"at" doc:"RFC3339 instant; omit for the live portfolio"`
}

func (d Deps) portfolioFor(ctx context.Context, in *portfolioInput) (portfolio.Portfolio, error) {
	if in == nil || in.At == "" {
		return d.load(ctx)
	}
	at, err := time.Parse(time.RFC3339, in.At)
	if err != nil {
		return portfolio.Portfolio{}, huma.Error400BadRequest("at must be an RFC3339 instant")
	}
	return d.loadAt(ctx, at)
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
		Description: "Without `at` this is the live projection. With `at` it is the ledger" +
			" folded to that instant and priced from a run rebuilt out of price_ticks --" +
			" never stored, and never reaching past a gap to a later price.",
	}, func(ctx context.Context, in *portfolioInput) (*struct{ Body portfolioBody }, error) {
		p, err := d.portfolioFor(ctx, in)
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
			Envelope:       envelopeOf(p),
			TotalValueUSD:  nullText(p.TotalValueUSD),
			Valuation:      renderValuation(p.Prices),
			UnpricedAssets: p.Unpriced,
			Positions:      renderPositions(p.Holdings),
			Balances:       renderBalances(p.Balances),
			Subtotals:      subtotals,
			Fees:           renderFees(p.Fees),
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

// nullText renders an optional money value. An absent one is "" rather than "0": a missing
// price and a price of zero are different claims, and collapsing them is how a deposit
// acquires a cost basis (L1).
func nullText(d decimal.NullDecimal) string {
	if !d.Valid {
		return ""
	}
	return d.Decimal.String()
}
