package valuation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/shopspring/decimal"
)

// EphemeralRunID is the id an unrecorded run carries. Zero rather than a negative sentinel
// because it is what an unset BIGINT looks like everywhere else, and a reader who ignores
// the field entirely gets an obviously-not-a-row value rather than a plausible one.
const EphemeralRunID int64 = 0

// At prices the registry as of an instant and returns the run without recording it.
//
// It writes nothing, and it does not need to: price_ticks is durable and the walk is pure,
// so the same instant yields the same run forever. Persisting one would grow a table by a
// row per curious click and add nothing an audit could not already reproduce (K48).
//
// The prices come from ListPricedPairs, which takes the newest tick at or *before* the
// instant. A gap in the ticks therefore surfaces as an old observed_at -- reported as
// price_stale -- and never as the next price after the gap. Reaching forward across a gap
// is look-ahead bias, and in a risk product it is the bug that makes a backtest look
// brilliant and a liquidation arrive unannounced.
//
// One thing it cannot promise, and says so rather than implying otherwise: the peg set is
// today's configuration (K17). A rebuilt run for a past instant assumes what we assume now,
// which is why the run it returns still carries AssumedPeg for the response to disclose.
func At(
	ctx context.Context, q *store.Queries, source string, at time.Time, pegs PegSet,
) (Run, error) {
	if source == "" {
		return Run{}, errors.New("valuation: a run must say where its prices came from")
	}
	walked, err := priceAll(ctx, q, at, pegs)
	if err != nil {
		return Run{}, err
	}
	if len(walked.prices) == 0 {
		return Run{}, fmt.Errorf("%w: %s", ErrNoRun, at.UTC().Format(time.RFC3339))
	}
	return Run{
		ID:               EphemeralRunID,
		AsOf:             at,
		Numeraire:        "USD",
		PriceSource:      source,
		AssumedPeg:       walked.anyAssumed,
		OldestObservedAt: walked.oldest,
		Prices:           walked.prices,
	}, nil
}

// DefaultPegAssets is what terminates every price path when nothing is configured. One
// asset, not two: the smaller the set, the more is priced through a real market instead of
// assumed (K17). USDT is deliberately absent, so a USDT depeg shows up in the numbers
// rather than being assumed away.
const DefaultPegAssets = "USDC"

// LoadPegs resolves a comma-separated list of peg symbols to asset ids.
//
// By symbol rather than by id, because configuration names assets the way a human does and
// an id is a database detail that differs between deployments. A symbol that does not
// resolve is an error rather than a skip: silently starting with an empty peg set would
// make every asset unpriceable and the cause invisible.
//
// It lives here rather than in either process because both need it -- the worker to produce
// runs and the API to rebuild one for `?at=` -- and two readings of one setting is how the
// same instant comes to have two answers.
func LoadPegs(ctx context.Context, q *store.Queries, configured string) (PegSet, error) {
	one := decimal.NewFromInt(1)
	pegs := PegSet{}
	for _, symbol := range strings.Split(configured, ",") {
		symbol = strings.TrimSpace(strings.ToUpper(symbol))
		if symbol == "" {
			continue
		}
		id, err := q.GetAssetIDBySymbol(ctx, symbol)
		if err != nil {
			return nil, fmt.Errorf("peg asset %q is not in the registry: %w", symbol, err)
		}
		pegs[id] = one
	}
	if len(pegs) == 0 {
		return nil, errors.New("no peg asset is configured; nothing could terminate a price path")
	}
	return pegs, nil
}
