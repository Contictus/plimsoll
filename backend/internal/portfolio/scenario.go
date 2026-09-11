// The I/O around the scenario engine: one read, one projection (M7.5).

package portfolio

import (
	"context"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/Contictus/plimsoll/backend/internal/scenario"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Scenario is the response: the account now, the account under the shock, and everything
// neither number could account for.
type Scenario struct {
	AsOf      time.Time
	Freshness freshness.Report
	Report    scenario.Report

	// Shocks is echoed back exactly as applied, because a projection read without knowing
	// which assets were moved is a number with no question attached.
	Shocks map[string]decimal.Decimal
}

// LoadScenario marks the account once and projects it under the shocks.
//
// One transaction and one valuation run, like every other response (K11, L10): a base case
// read through a different call than the shocked one would differ from the /risk page the
// user is looking at, and "every screen shows a different total" is exactly that.
func LoadScenario(
	ctx context.Context,
	db tenancy.Beginner,
	accountID uuid.UUID,
	w Window,
	collateralTTL time.Duration,
	shocks map[string]decimal.Decimal,
) (Scenario, error) {
	var (
		in        Input
		statuses  []ingest.Status
		wallet    decimal.Decimal
		positions []scenario.Position
		captures  []CollateralSnapshot
	)

	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		var err error
		if in, err = read(ctx, q, accountID, w); err != nil {
			return err
		}
		if statuses, err = ingest.StatusIn(ctx, q, accountID); err != nil {
			return err
		}

		snapshots, err := q.ListAccountCollateral(ctx, accountID)
		if err != nil {
			return fmt.Errorf("portfolio: read collateral for %s: %w", accountID, err)
		}
		for _, s := range snapshots {
			// The wallet balance, not the margin balance: the margin balance already carries
			// today's unrealized PnL, and the projection recomputes that at the shocked
			// price. Adding both would count the same profit twice.
			wallet = wallet.Add(s.WalletBalance)
			captures = append(captures, CollateralSnapshot{
				IntegrationID: s.IntegrationID,
				AsOf:          s.AsOf,
				CapturedAt:    s.CapturedAt,
			})
		}

		rows, err := q.ListCollateralPositions(ctx, accountID)
		if err != nil {
			return fmt.Errorf("portfolio: read collateral positions for %s: %w", accountID, err)
		}
		brackets, err := loadBrackets(ctx, q, accountID)
		if err != nil {
			return err
		}
		positions = make([]scenario.Position, 0, len(rows))
		for _, p := range rows {
			positions = append(positions, scenario.Position{
				Symbol:     p.CanonicalSymbol,
				BaseAsset:  p.BaseSymbol,
				Quantity:   p.Quantity,
				EntryPrice: p.EntryPrice,
				MarkPrice:  p.MarkPrice,
				Brackets:   brackets[p.InstrumentID],
			})
		}
		return nil
	})
	if err != nil {
		return Scenario{}, err
	}

	priced := Build(in)

	holdings := make([]scenario.Holding, 0, len(priced.Balances))
	for _, b := range priced.Balances {
		if b.Quantity.IsZero() {
			continue
		}
		holdings = append(holdings, scenario.Holding{
			Asset:    b.Asset,
			Quantity: b.Quantity,
			Price:    b.PriceUSD,
		})
	}

	report, err := scenario.Project(scenario.Input{
		WalletBalance: wallet,
		Positions:     positions,
		Holdings:      holdings,
		Shocks:        shocks,
	})
	if err != nil {
		return Scenario{}, err
	}

	// The same doubts every other response carries, plus the one that belongs to this
	// response in particular: a projection built on a stale capture is a projection of a
	// stale account, and a shock applied to last hour's positions is a confident answer to a
	// question about an account that has since changed (L11).
	reasons := append([]freshness.Reason{}, priced.Freshness.Reasons...)
	reasons = append(reasons, ReasonsFor(statuses, nil, w.Now, w.LeaseTTL)...)
	reasons = append(reasons, captureAge(statuses, captures, w.Now, collateralTTL)...)

	return Scenario{
		AsOf:      priced.AsOf,
		Freshness: freshness.New(reasons...),
		Report:    report,
		Shocks:    shocks,
	}, nil
}

// loadBrackets reads every captured tier table in one query and groups it by instrument. One
// read rather than one per position: twenty symbols would otherwise be twenty round trips
// inside a single request.
func loadBrackets(
	ctx context.Context, q *store.Queries, accountID uuid.UUID,
) (map[int64][]collateral.Bracket, error) {
	rows, err := q.ListAccountLeverageBrackets(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("portfolio: read leverage brackets for %s: %w", accountID, err)
	}
	out := map[int64][]collateral.Bracket{}
	for _, r := range rows {
		out[r.InstrumentID] = append(out[r.InstrumentID], collateral.Bracket{
			Bracket:          int(r.Bracket),
			NotionalFloor:    r.NotionalFloor,
			NotionalCap:      r.NotionalCap,
			MaintMarginRatio: r.MaintMarginRatio,
			Cum:              r.Cum,
		})
	}
	return out, nil
}

// captureAge says how old the margin picture under the projection is.
//
// An integration with no capture at all raises collateral_unavailable rather than being
// silently projected from a wallet balance of zero: an unknown margin picture and an empty one
// are opposite claims, and only one of them is safe to act on (K50, L11).
func captureAge(
	statuses []ingest.Status, captures []CollateralSnapshot, now time.Time, ttl time.Duration,
) []freshness.Reason {
	byIntegration := make(map[uuid.UUID]CollateralSnapshot, len(captures))
	for _, c := range captures {
		byIntegration[c.IntegrationID] = c
	}

	var out []freshness.Reason
	for _, s := range statuses {
		capture, ok := byIntegration[s.IntegrationID]
		name := s.Exchange + " " + s.Label
		if !ok {
			out = append(out, collateralUnavailable(name, now))
			continue
		}
		if now.Sub(capture.AsOf) > ttl {
			out = append(out, collateralStale(name, capture.AsOf))
		}
	}
	return out
}
