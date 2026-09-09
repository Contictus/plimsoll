package portfolio

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// RealizedChange is what an account closed out over an interval, in the asset it was
// denominated in. Per quote asset and never summed: realized PnL on BTC-USDT is USDT and on
// ETH-BTC is BTC, and one number holding both would have no unit (K11).
type RealizedChange struct {
	Asset       string
	RealizedPnL decimal.Decimal
}

// PnL is what happened between two instants: what was closed out, and what the open
// positions did.
//
// It carries two valuation runs, one per end, and that is not a breach of L10 -- it is what
// L10 protects. The law forbids one number built from two price sources; here every number
// names the end it came from, and no figure mixes them. A question about an interval cannot
// be answered from one instant's prices.
type PnL struct {
	From time.Time
	To   time.Time

	// AsOf is the later end. A response is as current as its most recent claim.
	AsOf time.Time

	Realized []RealizedChange

	// UnrealizedAtFrom and UnrealizedAtTo are the open positions marked at each end, in the
	// numeraire. Invalid when that end had no run: an interval with an unpriced end has no
	// change to report, and reporting one anyway would invent the half that is missing.
	UnrealizedAtFrom decimal.NullDecimal
	UnrealizedAtTo   decimal.NullDecimal

	TotalAtFrom decimal.NullDecimal
	TotalAtTo   decimal.NullDecimal

	AtFrom *PriceRun
	AtTo   *PriceRun

	Freshness freshness.Report
}

// LoadPnL answers "what did this account make between from and to".
//
// Both ends are read in one transaction, so the two halves of a difference cannot come from
// two different views of the ledger -- a subtraction across two reads is how a portfolio
// appears to have earned money that was only a concurrent write.
//
// Realized comes from the ledger: the fold's realized PnL at `to` less the same at `from`,
// per quote asset. It needs no prices at all and is exact.
func LoadPnL(
	ctx context.Context,
	db tenancy.Beginner,
	accountID uuid.UUID,
	from, to time.Time,
	w Window,
	pegs string,
) (PnL, error) {
	if !to.After(from) {
		return PnL{}, fmt.Errorf("portfolio: pnl needs from before to, got %s and %s",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}

	var start, end Portfolio
	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		startIn, err := readAt(ctx, q, accountID, from, w, pegs)
		if err != nil {
			return err
		}
		endIn, err := readAt(ctx, q, accountID, to, w, pegs)
		if err != nil {
			return err
		}
		start, end = Build(startIn), Build(endIn)
		return nil
	})
	if err != nil {
		return PnL{}, err
	}
	return diff(from, to, start, end), nil
}

// diff is the pure half: two portfolios in, the interval between them out. Separated so the
// arithmetic can be tested without a database (L4).
func diff(from, to time.Time, start, end Portfolio) PnL {
	out := PnL{
		From: from, To: to, AsOf: to,
		UnrealizedAtFrom: unrealized(start),
		UnrealizedAtTo:   unrealized(end),
		TotalAtFrom:      start.TotalValueUSD,
		TotalAtTo:        end.TotalValueUSD,
		AtFrom:           start.Prices,
		AtTo:             end.Prices,
		Realized:         realizedBetween(start, end),
	}

	// Both ends' reasons, each stamped with the end it came from. A caller reading only
	// `status` is then told about the worse of the two, which is the honest summary of a
	// number derived from both (L11).
	reasons := append(stamp(from, start.Freshness), stamp(to, end.Freshness)...)
	out.Freshness = freshness.New(reasons...)
	return out
}

// realizedBetween subtracts the fold's realized PnL at the earlier end from the later one,
// per quote asset. Position by position rather than subtotal by subtotal: a position that
// closed inside the interval is absent from neither end's fold but its realized PnL sits in
// only one of them, and subtracting aggregates would attribute it to the wrong asset if two
// quote assets moved at once.
func realizedBetween(start, end Portfolio) []RealizedChange {
	was := map[string]decimal.Decimal{}
	for _, h := range start.Holdings {
		was[h.ID()] = h.RealizedPnL
	}

	byAsset := map[string]decimal.Decimal{}
	for _, h := range end.Holdings {
		byAsset[h.QuoteAsset] = byAsset[h.QuoteAsset].Add(h.RealizedPnL.Sub(was[h.ID()]))
	}

	out := make([]RealizedChange, 0, len(byAsset))
	for asset, amount := range byAsset {
		out = append(out, RealizedChange{Asset: asset, RealizedPnL: amount})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset < out[j].Asset })
	return out
}

// unrealized sums what the open positions were worth over what they cost, in the numeraire.
// Positions the run could not price are absent from the sum and named in Unpriced, which is
// why this is a figure to read next to that list rather than on its own.
func unrealized(p Portfolio) decimal.NullDecimal {
	if p.Prices == nil {
		return decimal.NullDecimal{}
	}
	total := decimal.Zero
	for _, h := range p.Holdings {
		if h.UnrealizedPnL.Valid {
			total = total.Add(h.UnrealizedPnL.Decimal)
		}
	}
	return decimal.NullDecimal{Decimal: total.Round(moneyScale), Valid: true}
}

// stamp prefixes each reason with the end it describes, because the same code raised at both
// ends is two different facts and a reader has to be able to tell which one moved.
func stamp(at time.Time, report freshness.Report) []freshness.Reason {
	out := make([]freshness.Reason, 0, len(report.Reasons))
	for _, r := range report.Reasons {
		r.Detail = fmt.Sprintf("at %s: %s", at.UTC().Format(time.RFC3339), r.Detail)
		out = append(out, r)
	}
	return out
}
