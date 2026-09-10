package portfolio

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/strategy"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/google/uuid"
)

// Window is the clock and the tolerances one read is judged against: when it was asked, how
// long a worker's report stays believable, and how old a price may be before the response
// says so.
//
// One struct rather than three parameters because every read endpoint needs all three and a
// call site that passed them in the wrong order would still compile -- two durations next to
// each other is a defect waiting for a hurried afternoon.
type Window struct {
	Now      time.Time
	LeaseTTL time.Duration
	PriceTTL time.Duration
}

// Load reads one account's portfolio and everything needed to qualify it.
//
// Every read happens in one transaction, deliberately. A response built from positions read
// in one transaction and statuses read in another can contradict itself -- a position from
// after the status that describes it -- and "one response, one consistent view" is the same
// rule that makes a valuation run singular (K11, L10).
//
// The Window is a parameter rather than a clock and two constants, so the freshness ranking
// is testable without waiting for anything (L4).
func Load(
	ctx context.Context,
	db tenancy.Beginner,
	accountID uuid.UUID,
	w Window,
) (Portfolio, error) {
	var in Input
	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		var err error
		in, err = read(ctx, q, accountID, w)
		return err
	})
	if err != nil {
		return Portfolio{}, err
	}
	return Build(in), nil
}

// read gathers the rows. Split from Load so that a caller already inside a transaction --
// the lineage endpoint, which needs the same qualification for one position -- reads the
// same way rather than a second way.
func read(ctx context.Context, q *store.Queries, accountID uuid.UUID, w Window) (Input, error) {
	now, leaseTTL, priceTTL := w.Now, w.LeaseTTL, w.PriceTTL
	rows, err := q.ListAccountPositions(ctx, accountID)
	if err != nil {
		return Input{}, fmt.Errorf("portfolio: read positions for %s: %w", accountID, err)
	}
	feeRows, err := q.ListAccountPositionFees(ctx, accountID)
	if err != nil {
		return Input{}, fmt.Errorf("portfolio: read fees for %s: %w", accountID, err)
	}
	statuses, err := ingest.StatusIn(ctx, q, accountID)
	if err != nil {
		return Input{}, err
	}
	laggingIDs, err := q.ListLaggingIntegrations(ctx, accountID)
	if err != nil {
		return Input{}, fmt.Errorf("portfolio: read projection lag for %s: %w", accountID, err)
	}
	balanceRows, err := q.ListAccountBalances(ctx, accountID)
	if err != nil {
		return Input{}, fmt.Errorf("portfolio: read balances for %s: %w", accountID, err)
	}
	unattributedIDs, err := q.ListIntegrationsWithUnattributedFees(ctx, accountID)
	if err != nil {
		return Input{}, fmt.Errorf("portfolio: read unattributed fees for %s: %w", accountID, err)
	}
	feeAssetRows, err := q.ListAccountFeeAssets(ctx, accountID)
	if err != nil {
		return Input{}, fmt.Errorf("portfolio: read fee assets for %s: %w", accountID, err)
	}
	// Read in the same transaction as the positions it labels, so a tag applied between two
	// reads cannot label a position the response did not include (K11, L10).
	tags, err := strategy.Of(ctx, q, accountID)
	if err != nil {
		return Input{}, err
	}

	// One run, read inside the same transaction as everything else, so the prices and the
	// quantities they multiply are one consistent view (K11, L10). A missing run is not an
	// error: it is a portfolio without a total, and Build says so.
	var priced *valuation.Run
	run, err := valuation.LatestRun(ctx, q, now)
	switch {
	case err == nil:
		priced = &run
	case errors.Is(err, valuation.ErrNoRun):
	default:
		return Input{}, fmt.Errorf("portfolio: read valuation for %s: %w", accountID, err)
	}

	feeAssets := make([]FeeAsset, 0, len(feeAssetRows))
	for _, f := range feeAssetRows {
		feeAssets = append(feeAssets, FeeAsset{ID: f.ID, Symbol: f.CanonicalSymbol})
	}

	// Keyed by the projection's own key, so a fee cannot be attached to the wrong
	// integration's copy of the same instrument.
	type key struct {
		integration uuid.UUID
		instrument  int64
	}
	fees := map[key][]position.FeeTotal{}
	for _, f := range feeRows {
		k := key{f.IntegrationID, f.InstrumentID}
		fees[k] = append(fees[k], position.FeeTotal{Asset: f.FeeAsset, Amount: f.Amount})
	}

	lagging := make(map[uuid.UUID]bool, len(laggingIDs))
	for _, id := range laggingIDs {
		lagging[id] = true
	}
	unattributed := make(map[uuid.UUID]bool, len(unattributedIDs))
	for _, id := range unattributedIDs {
		unattributed[id] = true
	}

	balances := make([]Balance, 0, len(balanceRows))
	for _, b := range balanceRows {
		balances = append(balances, Balance{
			IntegrationID: b.IntegrationID,
			AssetID:       b.AssetID,
			Asset:         b.CanonicalSymbol,
			Quantity:      b.Quantity,
			LastEventTime: b.LastEventTime,
		})
	}

	positions := make([]Position, 0, len(rows))
	for _, r := range rows {
		tag := tags[strategy.PositionKey{IntegrationID: r.IntegrationID, InstrumentID: r.InstrumentID}]
		positions = append(positions, Position{
			Strategy:      tag.Name,
			StrategyID:    tag.ID,
			IntegrationID: r.IntegrationID,
			InstrumentID:  r.InstrumentID,
			Symbol:        r.CanonicalSymbol,
			Kind:          r.Kind,
			BaseAssetID:   r.BaseAssetID,
			QuoteAssetID:  r.QuoteAssetID,
			BaseAsset:     r.BaseAsset,
			QuoteAsset:    r.QuoteAsset,
			Quantity:      r.Quantity,
			AvgEntryPrice: r.AvgEntryPrice,
			RealizedPnL:   r.RealizedPnl,
			Fees:          fees[key{r.IntegrationID, r.InstrumentID}],
			LastEventTime: r.LastEventTime,
		})
	}

	// Ordered worst-cause first is not the point -- freshness.New ranks severity itself.
	// What matters is that every source of doubt is here, so a reader that trusts `status`
	// is trusting all of them at once (L11).
	// The valuation's own reasons are not here: Build raises them, because whether a total
	// exists at all is something only the marking-to-market knows.
	reasons := ReasonsFor(statuses, lagging, now, leaseTTL)
	reasons = append(reasons, NegativeBalances(balances)...)
	reasons = append(reasons, UnattributedFees(statuses, unattributed, now)...)

	return Input{
		AsOf:      now,
		Positions: positions,
		Balances:  balances,
		Reasons:   reasons,
		Valuation: priced,
		FeeAssets: feeAssets,
		PriceTTL:  priceTTL,
	}, nil
}
