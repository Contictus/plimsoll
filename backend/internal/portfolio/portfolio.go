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
	"github.com/Contictus/plimsoll/backend/internal/valuation"
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

	// BaseAssetID and QuoteAssetID are what a valuation run is keyed by. They are carried
	// alongside the symbols rather than resolved from them: a symbol is a label a reader
	// recognises, and looking an asset up by one here would be resolving today's mapping
	// against a position folded from events with their own event_time (L8).
	BaseAssetID  int64
	QuoteAssetID int64

	Quantity      decimal.Decimal
	AvgEntryPrice decimal.Decimal
	RealizedPnL   decimal.Decimal
	Fees          []position.FeeTotal

	// LastEventTime is the event time of the last fill folded into this row. It is the
	// honest answer to "how current is this position", and it is not the same as the
	// response's AsOf: one is the account's last activity, the other is when we looked.
	LastEventTime time.Time

	// Strategy is the group the user put this position in, empty when untagged. It is read
	// alongside the fold rather than stored on it: the tag is user input and `positions` is
	// dropped and rebuilt (K30). StrategyID is carried too, because a client that wants to
	// re-tag needs the id and the name is what a human reads.
	Strategy   string
	StrategyID uuid.UUID
}

// Holding is a Position with what Build derived from it.
type Holding struct {
	Position

	// CostBasis is what is tied up in this position, in the quote asset: the unsigned
	// quantity times the entry price. Unsigned because a short position paid for its
	// exposure too, and a signed cost basis would shrink as the account took on more risk.
	CostBasis decimal.Decimal

	// MarketValue is the position marked to market in the run's numeraire, signed: a short's
	// is negative, because it is exposure owed rather than money held. Invalid when the base
	// asset had no route to the numeraire -- absent, never zero, because a price we do not
	// have and a price of zero are different claims (L1).
	MarketValue decimal.NullDecimal

	// UnrealizedPnL is MarketValue less what the position cost, both converted at the same
	// run. Invalid unless both legs were priced: converting one leg at this run and the
	// other at anything else is the "every screen shows a different total" failure (L10).
	UnrealizedPnL decimal.NullDecimal

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

	// PriceUSD and ValueUSD come from the response's one valuation run. Both are invalid
	// when the run had no route to this asset, which is reported rather than rounded to
	// zero: a holding nobody could price is not a holding worth nothing.
	PriceUSD decimal.NullDecimal
	ValueUSD decimal.NullDecimal

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
	// AsOf is when the account was read. It is not necessarily the portfolio's as_of: once
	// a run backs the response that comes from the run, and this stays the read time, which
	// is what price staleness is measured against.
	AsOf      time.Time
	Positions []Position
	Balances  []Balance
	Reasons   []freshness.Reason

	// Valuation is the one run every number in this response is priced from, or nil when no
	// run has completed. One run per response, never two (K11, L10).
	Valuation *valuation.Run

	// FeeAssets are the assets this account has paid fees in, so a fee the run cannot
	// express in the numeraire is disclosed rather than left for a reader to add up into a
	// number with no unit (L9).
	FeeAssets []FeeAsset

	// PriceTTL is how old the run's worst leg may be before the response says so. A
	// parameter rather than a constant because "stale" is a product decision, and one this
	// package must be able to test without waiting (L4).
	PriceTTL time.Duration
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

	// TotalValueUSD is the sum of the valued balances, and invalid when no run backs the
	// response. It excludes anything in Unpriced -- which is why Unpriced is part of the
	// answer and not a footnote: "worth X" and "worth X, minus the part we could not price"
	// are different sentences and only one of them is true.
	TotalValueUSD decimal.NullDecimal

	// Unpriced names every asset held in a non-zero quantity that the run could not route
	// to the numeraire, sorted so two reads compare.
	Unpriced []string

	// Prices is the run this response was built from, nil when there is none.
	Prices *PriceRun
}

// Build derives the holdings, the per-quote subtotals and the portfolio-wide fee totals.
//
// The output is ordered and does not depend on the order of the input: two reads of the
// same rows produce byte-identical output, which is what makes a diff between them mean
// something.
func Build(in Input) Portfolio {
	out := Portfolio{
		AsOf:     in.AsOf,
		Holdings: make([]Holding, 0, len(in.Positions)),
		Balances: make([]Balance, 0, len(in.Balances)),
		ByQuote:  make([]QuoteTotal, 0),
		Fees:     make([]position.FeeTotal, 0),
		Unpriced: make([]string, 0),
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

	// Valuation runs last, over the sorted slices, so what it marks is what the response
	// serves in the order the response serves it. Its reasons join the caller's rather than
	// replacing them: a stale price and a stalled ingest are both true at once, and a reader
	// who acts on only one of them has been told half the story (L11).
	reasons := make([]freshness.Reason, 0, len(in.Reasons)+4)
	reasons = append(reasons, in.Reasons...)
	reasons = append(reasons, value(&out, in)...)
	out.Freshness = freshness.New(reasons...)
	return out
}
