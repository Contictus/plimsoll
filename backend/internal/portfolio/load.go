package portfolio

import (
	"context"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
)

// Load reads one account's portfolio and everything needed to qualify it.
//
// Every read happens in one transaction, deliberately. A response built from positions read
// in one transaction and statuses read in another can contradict itself -- a position from
// after the status that describes it -- and "one response, one consistent view" is the same
// rule that makes a valuation run singular (K11, L10).
//
// now and leaseTTL are parameters rather than a clock and a constant, so the freshness
// ranking is testable without waiting for anything (L4).
func Load(
	ctx context.Context,
	db tenancy.Beginner,
	accountID uuid.UUID,
	now time.Time,
	leaseTTL time.Duration,
) (Portfolio, error) {
	var in Input
	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		var err error
		in, err = read(ctx, q, accountID, now, leaseTTL)
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
func read(
	ctx context.Context,
	q *store.Queries,
	accountID uuid.UUID,
	now time.Time,
	leaseTTL time.Duration,
) (Input, error) {
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
		positions = append(positions, Position{
			IntegrationID: r.IntegrationID,
			InstrumentID:  r.InstrumentID,
			Symbol:        r.CanonicalSymbol,
			Kind:          r.Kind,
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
	reasons := []freshness.Reason{ValuationUnavailable(now)}
	reasons = append(reasons, ReasonsFor(statuses, lagging, now, leaseTTL)...)
	reasons = append(reasons, NegativeBalances(balances)...)
	reasons = append(reasons, UnattributedFees(statuses, unattributed, now)...)

	return Input{
		AsOf:      now,
		Positions: positions,
		Balances:  balances,
		Reasons:   reasons,
	}, nil
}
