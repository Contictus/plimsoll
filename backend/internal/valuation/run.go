package valuation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/shopspring/decimal"
)

// This file is the one place in the package that touches a database, and it holds the same
// line internal/ledger holds: a transaction-scoped *store.Queries handed in by the caller,
// never a pool, never a clock, never a logger (L4). The arithmetic stays in path.go, where
// it can be exercised without Docker; what lives here is reading the graph and writing what
// the walk found.

// Result is what one run turned out to be.
type Result struct {
	RunID int64

	// Priced and Unpriceable partition the registry. An asset that could not be routed to
	// the numeraire is counted rather than dropped: the caller turns it into "worth X,
	// minus the part we could not price", which is a true sentence, where silently
	// omitting it would make "worth X" a false one.
	Priced      int
	Unpriceable []int64
}

// Produce prices every asset in the registry as of an instant and records the run.
//
// The whole run is written inside the caller's transaction, which is what makes a run
// atomic: the run row and its prices commit together, so a reader can never see a run that
// is half-priced. There is deliberately no "completed" flag -- the row existing is what
// completion means, and a flag would be a second thing to get wrong.
//
// as_of is a parameter rather than time.Now(), so the same function produces the live run
// and rebuilds one for a past instant from price_ticks.
func Produce(
	ctx context.Context,
	q *store.Queries,
	source string,
	asOf time.Time,
	pegs PegSet,
) (Result, error) {
	if source == "" {
		return Result{}, errors.New("valuation: a run must say where its prices came from")
	}
	if len(pegs) == 0 {
		// Without something to terminate the walk every asset is unpriceable, and a run of
		// nothing but failures is worse than no run: it looks like an answer.
		return Result{}, errors.New("valuation: a run needs at least one assumed asset to terminate its paths")
	}

	rates, err := loadRates(ctx, q, asOf)
	if err != nil {
		return Result{}, err
	}
	assets, err := q.ListAssetIDs(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("valuation: read asset registry: %w", err)
	}

	priced := make([]store.InsertValuationPriceParams, 0, len(assets))
	var (
		out            Result
		anyAssumed     bool
		oldestObserved time.Time
	)

	for _, assetID := range assets {
		p, err := PriceOf(assetID, rates, pegs)
		if errors.Is(err, ErrNoRoute) {
			out.Unpriceable = append(out.Unpriceable, assetID)
			continue
		}
		if err != nil {
			return Result{}, fmt.Errorf("valuation: price asset %d: %w", assetID, err)
		}

		path, err := json.Marshal(p.Path)
		if err != nil {
			return Result{}, fmt.Errorf("valuation: encode path for asset %d: %w", assetID, err)
		}
		anyAssumed = anyAssumed || p.AssumedPeg
		if !p.OldestObservedAt.IsZero() &&
			(oldestObserved.IsZero() || p.OldestObservedAt.Before(oldestObserved)) {
			oldestObserved = p.OldestObservedAt
		}

		priced = append(priced, store.InsertValuationPriceParams{
			AssetID:    assetID,
			PriceUsd:   p.USD,
			Path:       path,
			AssumedPeg: p.AssumedPeg,
			ObservedAt: nullTime(p.OldestObservedAt),
		})
	}

	if len(priced) == 0 {
		// Nothing could be priced at all. Recording an empty run would give a reader
		// something that looks like a valuation and answers nothing.
		return Result{}, fmt.Errorf("valuation: no asset could be priced as of %s",
			asOf.UTC().Format(time.RFC3339))
	}

	runID, err := q.InsertValuationRun(ctx, store.InsertValuationRunParams{
		AsOf:             asOf,
		PriceSource:      source,
		AssumedPeg:       anyAssumed,
		OldestObservedAt: nullTime(oldestObserved),
	})
	if err != nil {
		return Result{}, fmt.Errorf("valuation: record run: %w", err)
	}

	for i := range priced {
		priced[i].RunID = runID
		if err := q.InsertValuationPrice(ctx, priced[i]); err != nil {
			return Result{}, fmt.Errorf("valuation: record price for asset %d: %w",
				priced[i].AssetID, err)
		}
	}

	out.RunID = runID
	out.Priced = len(priced)
	return out, nil
}

// loadRates reads the price graph as it stood at an instant.
func loadRates(ctx context.Context, q *store.Queries, asOf time.Time) (*Rates, error) {
	rows, err := q.ListPricedPairs(ctx, asOf)
	if err != nil {
		return nil, fmt.Errorf("valuation: read priced pairs: %w", err)
	}
	pairs := make([]Pair, 0, len(rows))
	for _, r := range rows {
		pairs = append(pairs, Pair{
			InstrumentID: r.InstrumentID,
			BaseAssetID:  r.BaseAssetID,
			QuoteAssetID: r.QuoteAssetID,
			Price:        r.Price,
			ObservedAt:   r.ObservedAt,
		})
	}
	return NewRates(pairs), nil
}

// Run is a recorded valuation, as a reader gets it back.
type Run struct {
	ID               int64
	AsOf             time.Time
	Numeraire        string
	PriceSource      string
	AssumedPeg       bool
	OldestObservedAt time.Time
	Prices           map[int64]RecordedPrice
}

// RecordedPrice is one asset's price in a run, with the arithmetic that produced it.
type RecordedPrice struct {
	AssetID    int64
	USD        decimal.Decimal
	Path       []Hop
	AssumedPeg bool
	ObservedAt time.Time
}

// ErrNoRun means nothing has been valued at or before the instant asked for.
//
// Distinct from a run that priced nothing, and the difference matters to a reader: one says
// "the feed has never run", the other says "the feed ran and could not price your assets".
var ErrNoRun = errors.New("valuation: no run at or before this instant")

// LatestRun reads the newest run at or before an instant, with its prices.
//
// At or before, never after: a response for a past instant built on prices from after it is
// look-ahead bias, which in a risk product is the bug that makes a backtest look brilliant
// and a liquidation arrive unannounced.
func LatestRun(ctx context.Context, q *store.Queries, at time.Time) (Run, error) {
	row, err := q.GetLatestValuationRun(ctx, at)
	if err != nil {
		return Run{}, fmt.Errorf("%w: %s", ErrNoRun, at.UTC().Format(time.RFC3339))
	}

	out := Run{
		ID: row.ID, AsOf: row.AsOf, Numeraire: row.Numeraire,
		PriceSource: row.PriceSource, AssumedPeg: row.AssumedPeg,
		Prices: map[int64]RecordedPrice{},
	}
	if row.OldestObservedAt != nil {
		out.OldestObservedAt = *row.OldestObservedAt
	}

	prices, err := q.ListValuationPrices(ctx, row.ID)
	if err != nil {
		return Run{}, fmt.Errorf("valuation: read prices of run %d: %w", row.ID, err)
	}
	for _, p := range prices {
		var path []Hop
		if err := json.Unmarshal(p.Path, &path); err != nil {
			return Run{}, fmt.Errorf("valuation: decode path of asset %d in run %d: %w",
				p.AssetID, row.ID, err)
		}
		recorded := RecordedPrice{
			AssetID: p.AssetID, USD: p.PriceUsd, Path: path, AssumedPeg: p.AssumedPeg,
		}
		if p.ObservedAt != nil {
			recorded.ObservedAt = *p.ObservedAt
		}
		out.Prices[p.AssetID] = recorded
	}
	return out, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
