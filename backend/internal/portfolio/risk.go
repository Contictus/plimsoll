package portfolio

import (
	"context"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/collateral"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/Contictus/plimsoll/backend/internal/risk"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// CollateralSnapshot is one integration's stored capture, as read.
type CollateralSnapshot struct {
	IntegrationID uuid.UUID
	AsOf          time.Time
	CapturedAt    time.Time

	MarginBalance     decimal.Decimal
	WalletBalance     decimal.Decimal
	UnrealizedPnL     decimal.Decimal
	MaintenanceMargin decimal.Decimal
	AvailableBalance  decimal.Decimal
}

// CollateralPosition is one position as the venue reported it at the capture.
type CollateralPosition struct {
	IntegrationID    uuid.UUID
	InstrumentID     int64
	Symbol           string
	Quantity         decimal.Decimal
	EntryPrice       decimal.Decimal
	MarkPrice        decimal.Decimal
	LiquidationPrice decimal.Decimal
	Notional         decimal.Decimal
	Leverage         decimal.Decimal
	MaintMargin      decimal.Decimal
}

// RiskInput is everything one /risk response is built from.
type RiskInput struct {
	Now           time.Time
	CollateralTTL time.Duration

	Statuses  []ingest.Status
	Snapshots []CollateralSnapshot
	Positions []CollateralPosition
	Reasons   []freshness.Reason
}

// RiskPosition is one position with the distance to its liquidation price.
type RiskPosition struct {
	CollateralPosition
	Distance decimal.NullDecimal
}

// IntegrationRisk is one integration's margin picture.
type IntegrationRisk struct {
	IntegrationID uuid.UUID
	Exchange      string
	Label         string
	AsOf          time.Time

	MarginBalance     decimal.Decimal
	WalletBalance     decimal.Decimal
	UnrealizedPnL     decimal.Decimal
	MaintenanceMargin decimal.Decimal
	AvailableBalance  decimal.Decimal
	MarginBuffer      decimal.Decimal

	Positions []RiskPosition
}

// Risk is the response.
type Risk struct {
	AsOf         time.Time
	Freshness    freshness.Report
	Integrations []IntegrationRisk
}

// BuildRisk turns one read into the response, and is pure so that the ranking of these
// conditions is testable without a database or a clock (L4, L11).
//
// An integration with no capture is omitted rather than zeroed, and raises
// collateral_unavailable. An integration whose capture has aged past CollateralTTL is served
// WITH collateral_stale: an hour-old liquidation distance is still the best answer anyone
// has, and withholding it at the moment it is being asked for helps no one.
func BuildRisk(in RiskInput) Risk {
	snapshots := make(map[uuid.UUID]CollateralSnapshot, len(in.Snapshots))
	for _, s := range in.Snapshots {
		snapshots[s.IntegrationID] = s
	}
	positions := map[uuid.UUID][]RiskPosition{}
	for _, p := range in.Positions {
		positions[p.IntegrationID] = append(positions[p.IntegrationID], RiskPosition{
			CollateralPosition: p,
			Distance:           collateral.Distance(p.MarkPrice, p.LiquidationPrice),
		})
	}

	reasons := make([]freshness.Reason, 0, len(in.Reasons)+len(in.Statuses))
	reasons = append(reasons, in.Reasons...)

	out := Risk{Integrations: make([]IntegrationRisk, 0, len(in.Statuses))}
	for _, st := range in.Statuses {
		name := fmt.Sprintf("%s %s", st.Exchange, st.Label)
		snapshot, ok := snapshots[st.IntegrationID]
		if !ok {
			// Only for an integration that is supposed to be running. A paused one has no
			// worker by the operator's own decision, and calling that an error would teach
			// a reader to ignore the code on the day it means something.
			if st.Configured == "active" {
				reasons = append(reasons, collateralUnavailable(name, in.Now))
			}
			continue
		}
		if in.CollateralTTL > 0 && in.Now.Sub(snapshot.AsOf) > in.CollateralTTL {
			reasons = append(reasons, collateralStale(name, snapshot.AsOf))
		}

		// The oldest snapshot in the response dates the whole of it: a screen is only as
		// current as its stalest number, and naming the newest would date it by its
		// luckiest one (L10).
		if out.AsOf.IsZero() || snapshot.AsOf.Before(out.AsOf) {
			out.AsOf = snapshot.AsOf
		}

		out.Integrations = append(out.Integrations, IntegrationRisk{
			IntegrationID:     st.IntegrationID,
			Exchange:          st.Exchange,
			Label:             st.Label,
			AsOf:              snapshot.AsOf,
			MarginBalance:     snapshot.MarginBalance,
			WalletBalance:     snapshot.WalletBalance,
			UnrealizedPnL:     snapshot.UnrealizedPnL,
			MaintenanceMargin: snapshot.MaintenanceMargin,
			AvailableBalance:  snapshot.AvailableBalance,
			MarginBuffer: collateral.Buffer(collateral.Snapshot{
				MarginBalance:     snapshot.MarginBalance,
				MaintenanceMargin: snapshot.MaintenanceMargin,
			}),
			Positions: positions[st.IntegrationID],
		})
	}

	if out.AsOf.IsZero() {
		// Nothing was captured, so there is no instant to name. The read time claims only
		// "this was asked now", and the response says in the same breath that it carries
		// no margin picture at all.
		out.AsOf = in.Now
	}
	out.Freshness = freshness.New(reasons...)
	return out
}

// collateralStale is Since the capture's own instant, which is knowable exactly: it is when
// the numbers stopped being current, not when we noticed.
func collateralStale(name string, asOf time.Time) freshness.Reason {
	return freshness.Reason{
		Code:     freshness.ReasonCollateralStale,
		Severity: freshness.SeverityWarn,
		Detail: fmt.Sprintf("%s: the margin picture was captured at %s and nothing has"+
			" refreshed it since", name, asOf.Format(time.RFC3339)),
		Since: asOf,
	}
}

func collateralUnavailable(name string, now time.Time) freshness.Reason {
	return freshness.Reason{
		Code:     freshness.ReasonCollateralUnavailable,
		Severity: freshness.SeverityError,
		Detail: fmt.Sprintf("%s: no margin capture exists, so how close this account is to"+
			" liquidation is unknown -- it is not zero, it is unknown", name),
		Since: now,
	}
}

// LoadRisk reads one account's margin picture: every integration's capture, the positions
// inside it, and what the ingestion says about the whole.
//
// One transaction, like every other read here: a buffer read in one transaction beside a
// position read in another can describe two different accounts, which is the failure L10
// prevents for prices and this endpoint would otherwise reproduce for margin.
func LoadRisk(
	ctx context.Context,
	db tenancy.Beginner,
	accountID uuid.UUID,
	w Window,
	collateralTTL time.Duration,
) (Risk, error) {
	var in RiskInput
	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		statuses, err := ingest.StatusIn(ctx, q, accountID)
		if err != nil {
			return err
		}
		snapshotRows, err := q.ListAccountCollateral(ctx, accountID)
		if err != nil {
			return fmt.Errorf("portfolio: read collateral for %s: %w", accountID, err)
		}
		positionRows, err := q.ListCollateralPositions(ctx, accountID)
		if err != nil {
			return fmt.Errorf("portfolio: read collateral positions for %s: %w", accountID, err)
		}

		snapshots := make([]CollateralSnapshot, 0, len(snapshotRows))
		for _, s := range snapshotRows {
			snapshots = append(snapshots, CollateralSnapshot{
				IntegrationID:     s.IntegrationID,
				AsOf:              s.AsOf,
				CapturedAt:        s.CapturedAt,
				MarginBalance:     s.MarginBalance,
				WalletBalance:     s.WalletBalance,
				UnrealizedPnL:     s.UnrealizedPnl,
				MaintenanceMargin: s.MaintenanceMargin,
				AvailableBalance:  s.AvailableBalance,
			})
		}
		positions := make([]CollateralPosition, 0, len(positionRows))
		for _, p := range positionRows {
			positions = append(positions, CollateralPosition{
				IntegrationID:    p.IntegrationID,
				InstrumentID:     p.InstrumentID,
				Symbol:           p.CanonicalSymbol,
				Quantity:         p.Quantity,
				EntryPrice:       p.EntryPrice,
				MarkPrice:        p.MarkPrice,
				LiquidationPrice: p.LiquidationPrice,
				Notional:         p.Notional,
				Leverage:         p.Leverage,
				MaintMargin:      p.MaintMargin,
			})
		}

		in = RiskInput{
			Now:           w.Now,
			CollateralTTL: collateralTTL,
			Statuses:      statuses,
			Snapshots:     snapshots,
			Positions:     positions,
			// The same ingestion reasons every other response carries: a margin picture
			// built while nothing is reading the account is qualified by that too.
			Reasons: ReasonsFor(statuses, nil, w.Now, w.LeaseTTL),
		}
		return nil
	})
	if err != nil {
		return Risk{}, err
	}
	return BuildRisk(in), nil
}

// FundingBySymbol is what funding did to one instrument over a window, and the events that
// say so.
type FundingBySymbol struct {
	IntegrationID uuid.UUID
	InstrumentID  int64
	Symbol        string
	Asset         string

	// Total is signed and stays signed: negative is funding paid, positive is funding
	// received. Taking the absolute value would turn every payment into a cost, which is
	// wrong for exactly the account that is on the profitable side of the rate.
	Total  decimal.Decimal
	Events int64

	FirstEventTime time.Time
	LastEventTime  time.Time
}

// Funding is the response: what the ledger says funding did between two instants.
type Funding struct {
	AsOf      time.Time
	Freshness freshness.Report
	From, To  time.Time
	BySymbol  []FundingBySymbol
}

// LoadFunding sums the funding payments in [from, to) per instrument.
//
// From the ledger, and needing no price at all: a funding payment is a cash flow in the
// settle asset that the venue already denominated, so this is exact -- the numbers agree
// with the events they name because they are those events added up (L2, L3).
func LoadFunding(
	ctx context.Context,
	db tenancy.Beginner,
	accountID uuid.UUID,
	from, to time.Time,
	w Window,
) (Funding, error) {
	out := Funding{AsOf: w.Now, From: from, To: to}
	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		statuses, err := ingest.StatusIn(ctx, q, accountID)
		if err != nil {
			return err
		}
		rows, err := q.SumFundingBySymbol(ctx, store.SumFundingBySymbolParams{
			AccountID: accountID, FromTime: from, ToTime: to,
		})
		if err != nil {
			return fmt.Errorf("portfolio: sum funding for %s: %w", accountID, err)
		}

		out.BySymbol = make([]FundingBySymbol, 0, len(rows))
		for _, r := range rows {
			var instrumentID int64
			if r.InstrumentID != nil {
				instrumentID = *r.InstrumentID
			}
			out.BySymbol = append(out.BySymbol, FundingBySymbol{
				IntegrationID:  r.IntegrationID,
				InstrumentID:   instrumentID,
				Symbol:         r.CanonicalSymbol,
				Asset:          r.Asset,
				Total:          r.Total,
				Events:         r.Events,
				FirstEventTime: r.FirstEventTime,
				LastEventTime:  r.LastEventTime,
			})
		}

		// An incomplete backfill matters more here than almost anywhere: a sum over a
		// window whose history has not finished arriving is not wrong about the events it
		// has, it is silent about the ones it does not (L11).
		out.Freshness = freshness.New(ReasonsFor(statuses, nil, w.Now, w.LeaseTTL)...)
		return nil
	})
	if err != nil {
		return Funding{}, err
	}
	return out, nil
}

// Exposure is the risk report plus what it was computed from: one valuation run, named in
// as_of, and the freshness of everything that fed it.
type Exposure struct {
	AsOf      time.Time
	Freshness freshness.Report

	// Equity is invalid when no run has completed. Invalid rather than zero, for the same
	// reason a portfolio without a run carries no total: an account nobody could price is not
	// an account worth nothing (L11).
	Equity decimal.NullDecimal
	Report risk.Report

	// Run is the valuation this was priced from, nil when none has completed. The alert
	// runner needs it: alerts evaluate on completed runs and not per tick (ARCHITECTURE
	// section 7), because a per-tick alert fires on prices that never entered a published
	// total -- a message about a number the user was never shown.
	Run *PriceRun

	// Collateral is each integration's margin picture as it stood, so a rule on the margin
	// buffer reads the same snapshot /risk renders.
	Collateral []CollateralSnapshot
}

// LoadExposure marks the account once and measures it, portfolio-wide and per strategy.
//
// One transaction and one run (K11, L10). The alternative -- reading the portfolio through
// one call and the exposure through another -- is two valuations of one account, and "every
// screen shows a different total" is precisely what that produces.
//
// Equity is the portfolio's valued total plus the open perpetual PnL the balances do not
// carry yet: a perp's profit is not in any wallet until it is realized, and leaving it out
// would overstate leverage for exactly the account that is winning.
func LoadExposure(
	ctx context.Context,
	db tenancy.Beginner,
	accountID uuid.UUID,
	w Window,
	collateralTTL time.Duration,
) (Exposure, error) {
	var (
		in        Input
		snapshots []CollateralSnapshot
		statuses  []ingest.Status
	)
	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		var err error
		if in, err = read(ctx, q, accountID, w); err != nil {
			return err
		}
		statuses, err = ingest.StatusIn(ctx, q, accountID)
		if err != nil {
			return err
		}
		rows, err := q.ListAccountCollateral(ctx, accountID)
		if err != nil {
			return fmt.Errorf("portfolio: read collateral for %s: %w", accountID, err)
		}
		snapshots = make([]CollateralSnapshot, 0, len(rows))
		for _, s := range rows {
			snapshots = append(snapshots, CollateralSnapshot{
				IntegrationID:     s.IntegrationID,
				AsOf:              s.AsOf,
				MarginBalance:     s.MarginBalance,
				MaintenanceMargin: s.MaintenanceMargin,
				UnrealizedPnL:     s.UnrealizedPnl,
			})
		}
		return nil
	})
	if err != nil {
		return Exposure{}, err
	}

	p := Build(in)
	out := Exposure{AsOf: p.AsOf, Freshness: p.Freshness, Run: p.Prices, Collateral: snapshots}
	reasons := append([]freshness.Reason{}, p.Freshness.Reasons...)

	positions := make([]risk.Position, 0, len(p.Holdings))
	for _, h := range p.Holdings {
		if h.Flat {
			// A closed position is kept for its realized PnL, and it is not exposure. Its
			// market value is zero, so including it would change no total -- but it would
			// put a row of zeroes in every strategy's net delta, and a screen listing
			// exposures that are not exposures is one nobody reads carefully.
			continue
		}
		positions = append(positions, risk.Position{
			Symbol:      h.Symbol,
			BaseAsset:   h.BaseAsset,
			Strategy:    h.Strategy,
			MarketValue: h.MarketValue,
		})
	}

	if p.TotalValueUSD.Valid {
		equity := p.TotalValueUSD.Decimal
		for _, s := range snapshots {
			equity = equity.Add(s.UnrealizedPnL)
			if collateralTTL > 0 && w.Now.Sub(s.AsOf) > collateralTTL {
				reasons = append(reasons, collateralStale(nameOf(statuses, s.IntegrationID), s.AsOf))
			}
		}
		out.Equity = decimal.NewNullDecimal(equity)
	}

	// Computed with whatever equity there is: with none, the ratios come back invalid and the
	// exposures are still exact. Refusing to answer at all would withhold the half we know.
	out.Report = risk.Compute(risk.Input{Equity: out.Equity.Decimal, Positions: positions})
	out.Freshness = freshness.New(reasons...)
	return out, nil
}

// nameOf labels an integration the way every other reason does, so one response does not call
// the same connection two different things.
func nameOf(statuses []ingest.Status, id uuid.UUID) string {
	for _, s := range statuses {
		if s.IntegrationID == id {
			return fmt.Sprintf("%s %s", s.Exchange, s.Label)
		}
	}
	return id.String()
}
