package portfolio

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/google/uuid"
)

// LoadAt answers "what did this account hold at T", folded from the ledger and priced from
// a run rebuilt out of price_ticks. Nothing it produces is stored (K48).
//
// Two things it is not, and both are the point:
//
// It is not the live projection read with an older timestamp. The fold runs here, over the
// events at or before T, so an account that opened its first position yesterday is empty
// last week rather than the same as today.
//
// It is a pure function of the ledger, price_ticks, T and the peg configuration. The same T
// therefore gives the same answer byte for byte, which is what makes a past total something
// a reader can check rather than merely read (L3).
//
// It is not lagging, ever. The live endpoint reports projection_lagging because a ticker
// stands between the ledger and the positions (K38); this one folds the ledger itself, so
// the answer is current with the events by construction and the reason would be a lie.
func LoadAt(
	ctx context.Context,
	db tenancy.Beginner,
	accountID uuid.UUID,
	at time.Time,
	w Window,
	pegs string,
) (Portfolio, error) {
	var in Input
	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		var err error
		in, err = readAt(ctx, q, accountID, at, w, pegs)
		return err
	})
	if err != nil {
		return Portfolio{}, err
	}
	return Build(in), nil
}

func readAt(
	ctx context.Context,
	q *store.Queries,
	accountID uuid.UUID,
	at time.Time,
	w Window,
	pegs string,
) (Input, error) {
	folded, err := projection.FoldAt(ctx, q, accountID, at)
	if err != nil {
		return Input{}, err
	}

	instruments, err := nameInstruments(ctx, q, folded)
	if err != nil {
		return Input{}, err
	}
	assets, err := nameAssets(ctx, q, folded)
	if err != nil {
		return Input{}, err
	}

	positions := make([]Position, 0, len(folded.Positions))
	for key, state := range folded.Positions {
		named, ok := instruments[key.InstrumentID]
		if !ok {
			// The fold produced an instrument the registry does not name. That is a broken
			// foreign key rather than a data-quality finding, so it is an error and not a
			// freshness reason: there is no honest partial answer to give.
			return Input{}, fmt.Errorf("portfolio: instrument %d is not in the registry",
				key.InstrumentID)
		}
		positions = append(positions, Position{
			IntegrationID: key.IntegrationID,
			InstrumentID:  key.InstrumentID,
			Symbol:        named.CanonicalSymbol,
			Kind:          named.Kind,
			BaseAssetID:   named.BaseAssetID,
			QuoteAssetID:  named.QuoteAssetID,
			BaseAsset:     named.BaseAsset,
			QuoteAsset:    named.QuoteAsset,
			Quantity:      state.Quantity,
			AvgEntryPrice: state.AvgEntryPrice,
			RealizedPnL:   state.RealizedPnL,
			Fees:          state.Fees,
			LastEventTime: state.Cursor.EventTime,
		})
	}

	balances := make([]Balance, 0, len(folded.Balances))
	for key, held := range folded.Balances {
		balances = append(balances, Balance{
			IntegrationID: key.IntegrationID,
			AssetID:       key.AssetID,
			Asset:         assets[key.AssetID],
			Quantity:      held.Quantity,
			LastEventTime: held.Cursor.EventTime,
		})
	}

	// The run is rebuilt at T rather than read from valuation_runs, and it is not stored.
	// A missing one is not an error: it is a portfolio at T with no total, and Build says so.
	var priced *valuation.Run
	pegSet, err := valuation.LoadPegs(ctx, q, pegs)
	if err != nil {
		return Input{}, fmt.Errorf("portfolio: peg set for %s: %w",
			at.UTC().Format(time.RFC3339), err)
	}
	run, err := valuation.At(ctx, q, rebuiltSource, at, pegSet)
	if err == nil {
		priced = &run
	}

	// Only reasons that are properties of T. The ingestion state is deliberately absent:
	// "no worker is reading this integration" is a fact about now, and a response about a
	// past instant that carried it would be answering a question nobody asked -- with a
	// `since` that moves every time the same instant is requested, which is the one thing a
	// reproducible answer cannot do. A client that wants the state of ingestion asks for the
	// live portfolio, which is one request away.
	//
	// No lagging map either: this fold is the answer, so nothing can be behind it. Fee
	// assets are left out too -- that query spans the whole ledger, and a fee paid after T
	// is not a fault of a response about T.
	reasons := NegativeBalances(balances)

	return Input{
		// The prices are judged against the instant asked about, not the instant asked. A
		// gap in price_ticks around T is what price_stale means here, and it is the honest
		// report of one: the alternative is reaching past the gap to the next price, which
		// is look-ahead bias.
		AsOf:      at,
		Positions: positions,
		Balances:  balances,
		Reasons:   reasons,
		Valuation: priced,
		PriceTTL:  w.PriceTTL,
	}, nil
}

// rebuiltSource names where a rebuilt run's prices came from and that it was rebuilt. A
// reader comparing a past total against a recorded one must be able to tell which is which:
// a recorded run is what we said at the time, a rebuilt one is what the ticks say now.
const rebuiltSource = "price_ticks:rebuilt"

func nameInstruments(
	ctx context.Context, q *store.Queries, folded projection.At,
) (map[int64]store.ListInstrumentsByIDsRow, error) {
	ids := make([]int64, 0, len(folded.Positions))
	for key := range folded.Positions {
		ids = append(ids, key.InstrumentID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	rows, err := q.ListInstrumentsByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("portfolio: name instruments: %w", err)
	}
	out := make(map[int64]store.ListInstrumentsByIDsRow, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

func nameAssets(
	ctx context.Context, q *store.Queries, folded projection.At,
) (map[int64]string, error) {
	ids := make([]int64, 0, len(folded.Balances))
	for key := range folded.Balances {
		ids = append(ids, key.AssetID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	rows, err := q.ListAssetsByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("portfolio: name assets: %w", err)
	}
	out := make(map[int64]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.CanonicalSymbol
	}
	return out, nil
}
