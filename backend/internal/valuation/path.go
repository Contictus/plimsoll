package valuation

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// edge is one directed use of a pair: from one asset to the other, with the rate that
// direction requires.
type edge struct {
	to           int64
	instrumentID int64
	rate         decimal.Decimal
	inverted     bool
	observedAt   time.Time
}

// Rates is the set of pairs available at one instant, indexed for walking.
//
// Built once and read many times: a valuation run prices every asset an account holds
// against the same Rates, which is what makes "one run per response" mean the legs agree
// with each other as well as with the total (K11, L10).
type Rates struct {
	// out is the edges leaving each asset, sorted so a walk is deterministic. Map iteration
	// order is the classic way to lose that, and losing it means the total moves between
	// two reads for a reason no lineage can explain.
	out map[int64][]edge
}

// NewRates indexes pairs for walking. A pair quoted at zero or below is dropped: it cannot
// be inverted, and it is not a market. Dropping it here rather than at use means the walk
// routes around it exactly as it would route around a pair that does not exist — and
// ErrNoRoute then says so, instead of a division producing infinity.
func NewRates(pairs []Pair) *Rates {
	r := &Rates{out: map[int64][]edge{}}
	for _, p := range pairs {
		if p.Price.Sign() <= 0 {
			continue
		}
		// base -> quote: one base costs Price quote.
		r.out[p.BaseAssetID] = append(r.out[p.BaseAssetID], edge{
			to: p.QuoteAssetID, instrumentID: p.InstrumentID,
			rate: p.Price, observedAt: p.ObservedAt,
		})
		// quote -> base: the same pair used against its quotation, so the rate is inverted.
		r.out[p.QuoteAssetID] = append(r.out[p.QuoteAssetID], edge{
			to: p.BaseAssetID, instrumentID: p.InstrumentID,
			rate:     decimal.NewFromInt(1).DivRound(p.Price, moneyScale),
			inverted: true, observedAt: p.ObservedAt,
		})
	}
	for asset := range r.out {
		edges := r.out[asset]
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].to != edges[j].to {
				return edges[i].to < edges[j].to
			}
			return edges[i].instrumentID < edges[j].instrumentID
		})
		r.out[asset] = edges
	}
	return r
}

// PriceOf values one asset in USD and returns the path it used.
//
// The walk is breadth-first, so the route found is the one with the fewest market hops --
// fewest hops means fewest prices to be wrong, and fewest ages to be stale. Ties are broken
// by asset id and then instrument id, both ascending, so the same inputs always produce the
// same route: a total that moved because a map iterated differently would be a movement no
// user can see and no lineage can explain.
func PriceOf(assetID int64, rates *Rates, pegs PegSet) (Priced, error) {
	if rate, assumed := pegs[assetID]; assumed {
		// The asset is itself the assumption. One hop, no market, and flagged -- holding it
		// means the whole value rests on the assumption rather than on a price.
		return Priced{
			USD:        rate,
			Path:       []Hop{pegHop(assetID, rate)},
			AssumedPeg: true,
		}, nil
	}
	if rates == nil {
		return Priced{}, fmt.Errorf("%w: asset %d", ErrNoRoute, assetID)
	}

	// from[asset] is the edge that first reached it, which is enough to walk the route back
	// once a peg is found. BFS visits each asset once, so the first arrival is on a
	// shortest path.
	from := map[int64]edge{}
	origin := map[int64]int64{}
	seen := map[int64]bool{assetID: true}
	queue := []int64{assetID}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, e := range rates.out[current] {
			if seen[e.to] {
				continue
			}
			seen[e.to] = true
			from[e.to] = e
			origin[e.to] = current

			if rate, assumed := pegs[e.to]; assumed {
				return build(assetID, e.to, rate, from, origin), nil
			}
			queue = append(queue, e.to)
		}
	}
	return Priced{}, fmt.Errorf("%w: asset %d", ErrNoRoute, assetID)
}

// build walks the route back from the peg to the asset, then forward to apply it.
func build(assetID, pegAsset int64, pegRate decimal.Decimal, from map[int64]edge, origin map[int64]int64) Priced {
	var reversed []Hop
	for node := pegAsset; node != assetID; node = origin[node] {
		e := from[node]
		reversed = append(reversed, Hop{
			From: origin[node], To: e.to, InstrumentID: e.instrumentID,
			Rate: e.rate, Inverted: e.inverted, ObservedAt: e.observedAt,
		})
	}

	out := Priced{
		USD:        decimal.NewFromInt(1),
		Path:       make([]Hop, 0, len(reversed)+1),
		AssumedPeg: true,
	}
	for i := len(reversed) - 1; i >= 0; i-- {
		hop := reversed[i]
		out.USD = out.USD.Mul(hop.Rate)
		if out.OldestObservedAt.IsZero() || hop.ObservedAt.Before(out.OldestObservedAt) {
			out.OldestObservedAt = hop.ObservedAt
		}
		out.Path = append(out.Path, hop)
	}

	out.USD = out.USD.Mul(pegRate).Round(moneyScale)
	out.Path = append(out.Path, pegHop(pegAsset, pegRate))
	return out
}

// pegHop is the assumed leg that terminates every path. It carries no instrument and no
// observation, because it is not a market -- and saying that in the data is what lets a
// reader tell an assumption from a price.
func pegHop(assetID int64, rate decimal.Decimal) Hop {
	return Hop{From: assetID, To: 0, Rate: rate, Assumed: true}
}
