// Package portfolio turns one account's folded positions into what a reader is shown.
//
// Build is a pure function of rows and a clock reading (L4): no database, no prices, no
// logger. That is what lets the same code answer "what do I hold now" and, once M4 records
// price paths, "what did I hold at T" -- and what keeps its tests Docker-free.
//
// It deliberately produces no portfolio total. Realized PnL on BTC-USDT is denominated in
// USDT and on ETH-BTC in BTC; adding them makes a number with no unit. Until a valuation
// run exists there are subtotals per quote asset and a freshness reason saying why there is
// nothing more (K11, L10, L11).
package portfolio

import (
	"sort"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// moneyScale matches NUMERIC(38,18), the same scale internal/position rounds at. Derived
// numbers round here too, so what the engine computes and what Postgres would store are the
// same number (L1).
const moneyScale = 18

// Position is one folded position with the identity needed to name it. It is the loader's
// output and Build's input; the fields above CostBasis come straight from the projection,
// and everything Build adds is derived from them.
type Position struct {
	IntegrationID uuid.UUID
	InstrumentID  int64

	Symbol     string
	Kind       string
	BaseAsset  string
	QuoteAsset string

	Quantity      decimal.Decimal
	AvgEntryPrice decimal.Decimal
	RealizedPnL   decimal.Decimal
	Fees          []position.FeeTotal

	// LastEventTime is the event time of the last fill folded into this row. It is the
	// honest answer to "how current is this position", and it is not the same as the
	// response's AsOf: one is the account's last activity, the other is when we looked.
	LastEventTime time.Time
}

// Holding is a Position with what Build derived from it.
type Holding struct {
	Position

	// CostBasis is what is tied up in this position, in the quote asset: the unsigned
	// quantity times the entry price. Unsigned because a short position paid for its
	// exposure too, and a signed cost basis would shrink as the account took on more risk.
	CostBasis decimal.Decimal

	// Flat means the quantity is zero: a closed position, kept because its realized PnL and
	// its fees are still the account's. Dropping it would lose them; hiding the flag would
	// fill a dashboard with rows the user closed months ago.
	Flat bool
}

// Balance is how much of one asset an integration holds. It is the other half of a
// portfolio: a position says what an exposure cost, a balance says what is actually there,
// and for a spot account the second is the question being asked.
type Balance struct {
	IntegrationID uuid.UUID
	AssetID       int64
	Asset         string
	Quantity      decimal.Decimal
	LastEventTime time.Time

	// Negative means the ledger implies holding less than nothing, which cannot be true of
	// an exchange account. It is surfaced as a field and as a freshness reason: the field
	// so a client can mark the row, the reason so a client that reads nothing but status is
	// still warned (K14, L11).
	Negative bool
}

// QuoteTotal is the subtotal for everything denominated in one asset. It is a subtotal and
// never a total, and the type name says so on purpose.
type QuoteTotal struct {
	Asset       string
	CostBasis   decimal.Decimal
	RealizedPnL decimal.Decimal
}

// Input is everything Build needs. Reasons are gathered by the caller, which is the only
// part that has to touch the database.
type Input struct {
	AsOf      time.Time
	Positions []Position
	Balances  []Balance
	Reasons   []freshness.Reason
}

// Portfolio is one account's holdings as of one instant.
type Portfolio struct {
	// AsOf is when this was read. Once a valuation run backs the response it comes from the
	// run instead, and both will be the same field so a client never has to ask which (L10).
	AsOf time.Time

	Holdings  []Holding
	Balances  []Balance
	ByQuote   []QuoteTotal
	Fees      []position.FeeTotal
	Freshness freshness.Report
}

// Build derives the holdings, the per-quote subtotals and the portfolio-wide fee totals.
//
// The output is ordered and does not depend on the order of the input: two reads of the
// same rows produce byte-identical output, which is what makes a diff between them mean
// something.
func Build(in Input) Portfolio {
	out := Portfolio{
		AsOf:      in.AsOf,
		Holdings:  make([]Holding, 0, len(in.Positions)),
		Balances:  make([]Balance, 0, len(in.Balances)),
		ByQuote:   make([]QuoteTotal, 0),
		Fees:      make([]position.FeeTotal, 0),
		Freshness: freshness.New(in.Reasons...),
	}

	for _, b := range in.Balances {
		b.Negative = b.Quantity.IsNegative()
		out.Balances = append(out.Balances, b)
	}
	sort.Slice(out.Balances, func(i, j int) bool {
		a, b := out.Balances[i], out.Balances[j]
		if a.Asset != b.Asset {
			return a.Asset < b.Asset
		}
		return a.IntegrationID.String() < b.IntegrationID.String()
	})

	quotes := map[string]QuoteTotal{}
	fees := map[string]decimal.Decimal{}

	for _, p := range in.Positions {
		basis := p.Quantity.Abs().Mul(p.AvgEntryPrice).Round(moneyScale)
		out.Holdings = append(out.Holdings, Holding{
			Position:  p,
			CostBasis: basis,
			Flat:      p.Quantity.IsZero(),
		})

		q := quotes[p.QuoteAsset]
		q.Asset = p.QuoteAsset
		q.CostBasis = q.CostBasis.Add(basis)
		q.RealizedPnL = q.RealizedPnL.Add(p.RealizedPnL)
		quotes[p.QuoteAsset] = q

		for _, f := range p.Fees {
			fees[f.Asset] = fees[f.Asset].Add(f.Amount)
		}
	}

	for _, q := range quotes {
		out.ByQuote = append(out.ByQuote, q)
	}
	sort.Slice(out.ByQuote, func(i, j int) bool { return out.ByQuote[i].Asset < out.ByQuote[j].Asset })

	for asset, amount := range fees {
		out.Fees = append(out.Fees, position.FeeTotal{Asset: asset, Amount: amount})
	}
	sort.Slice(out.Fees, func(i, j int) bool { return out.Fees[i].Asset < out.Fees[j].Asset })

	// Sorted by what a reader recognises -- the pair -- and broken by integration and
	// instrument so two connections holding the same pair keep a stable order between them.
	sort.Slice(out.Holdings, func(i, j int) bool {
		a, b := out.Holdings[i], out.Holdings[j]
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.IntegrationID != b.IntegrationID {
			return a.IntegrationID.String() < b.IntegrationID.String()
		}
		return a.InstrumentID < b.InstrumentID
	})
	return out
}
