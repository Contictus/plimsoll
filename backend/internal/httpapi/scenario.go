package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/scenario"
	"github.com/danielgtaylor/huma/v2"
	"github.com/shopspring/decimal"
)

// outcomeBody is the account at one price. Every number is a string (L1), and the two that
// can be absent are EMPTY rather than "0": a maintenance requirement nobody could compute and
// one of zero are opposite claims, and a buffer rendered as zero reads as "right on the line"
// when it means "we do not know" (L11, K56).
type outcomeBody struct {
	Equity            string `json:"equity"`
	UnrealizedPnl     string `json:"unrealized_pnl"`
	MarginBalance     string `json:"margin_balance"`
	MaintenanceMargin string `json:"maintenance_margin" doc:"empty when a bracket table was never captured"`
	Buffer            string `json:"buffer"             doc:"signed and never clamped; empty when the requirement is unknown"`
	Liquidated        bool   `json:"liquidated"         doc:"false when the buffer could not be computed, in either direction"`
}

type scenarioBody struct {
	Base    outcomeBody `json:"base"`
	Shocked outcomeBody `json:"shocked"`

	// Shocks is echoed back exactly as applied. A projection read without knowing which
	// assets moved is a number with no question attached -- and the assets NOT listed here
	// held still, which is half of what the answer means (K56).
	Shocks map[string]string `json:"shocks"`

	// Unavailable names every position whose requirement could not be computed and every
	// holding that could not be priced.
	Unavailable []string `json:"unavailable"`

	freshness.Envelope
}

type scenarioInput struct {
	Body struct {
		// Shocks is a signed decimal fraction per asset, as a string: "-0.2" is minus twenty
		// percent. A string because it is arithmetic on money, and a float here would decide
		// a liquidation in the digits nobody checked (L1).
		Shocks map[string]string `json:"shocks" required:"true" doc:"asset -> signed fraction, e.g. {\"BTC\":\"-0.2\"}"`
	}
}

func renderOutcome(o scenario.Outcome) outcomeBody {
	out := outcomeBody{
		Equity:        o.Equity.String(),
		UnrealizedPnl: o.UnrealizedPnL.String(),
		MarginBalance: o.MarginBalance.String(),
		Liquidated:    o.Liquidated,
	}
	if o.MaintenanceMargin.Valid {
		out.MaintenanceMargin = o.MaintenanceMargin.Decimal.String()
	}
	if o.Buffer.Valid {
		out.Buffer = o.Buffer.Decimal.String()
	}
	return out
}

func (d Deps) registerScenario(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-risk-scenario",
		Method:      http.MethodPost,
		Path:        "/risk/scenario",
		Summary:     "What happens to this account if the price moves",
		Description: "A shock names its asset and the unshocked hold still: this endpoint" +
			" invents no correlations. A shock hits the spot holding and the perpetual" +
			" together, because they are the same asset -- a hedged book must not be shown a" +
			" loss it does not have. Maintenance is recomputed from the venue's tier table at" +
			" the shocked notional, never scaled from today's figure, because a shock worth" +
			" modelling usually crosses a tier. POST because it carries a body; it models, and" +
			" writes nothing.",
	}, func(ctx context.Context, in *scenarioInput) (*struct{ Body scenarioBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}

		shocks := make(map[string]decimal.Decimal, len(in.Body.Shocks))
		for asset, move := range in.Body.Shocks {
			parsed, err := decimal.NewFromString(move)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity(
					"shock for " + asset + " is not a number")
			}
			shocks[asset] = parsed
		}

		out, err := portfolio.LoadScenario(
			ctx, d.DB, accountID, d.window(), d.CollateralTTL, shocks)
		if errors.Is(err, scenario.ErrImpossibleMove) {
			// A price cannot go to zero or below. Refused rather than clamped: clamping
			// answers a question the caller did not ask and returns it as though it were the
			// one they did.
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		if err != nil {
			return nil, err
		}

		echoed := make(map[string]string, len(out.Shocks))
		for asset, move := range out.Shocks {
			echoed[asset] = move.String()
		}

		return &struct{ Body scenarioBody }{Body: scenarioBody{
			Base:        renderOutcome(out.Report.Base),
			Shocked:     renderOutcome(out.Report.Shocked),
			Shocks:      echoed,
			Unavailable: out.Report.Unavailable,
			Envelope:    freshness.Envelope{AsOf: out.AsOf, Freshness: out.Freshness},
		}}, nil
	})
}
