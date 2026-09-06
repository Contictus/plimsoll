package backfill

import (
	"context"
	"fmt"
	"sort"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
)

// Discover finds which symbols an account has ever traded, and opens a trade-walk scope for
// each one. It returns the symbols found, including those a previous run found, so a caller
// can hand them straight to WalkTrades.
//
// It probes every symbol once, with one weight-20 request each. That is not clever, and it
// is the only version with no hole in it (F4): there is no endpoint returning spot trades
// across symbols, and every cheaper discovery misses the same case -- an asset bought and
// fully sold leaves no trace in current balances, in deposits, or in withdrawals. Its only
// record is its trades, which cannot be found without already knowing the symbol, and a
// missing acquisition poisons the cost basis permanently (K26).
//
// One hole remains and is not closed here: exchangeInfo lists only currently listed
// symbols, so a pair delisted before the sweep cannot be probed because it can no longer be
// named. That is a gap in what the exchange will tell us, not in the sweep, and it belongs
// in freshness rather than in a comment pretending it does not exist.
//
// The sweep is resumable per symbol. Two to three thousand probes is 40-60k weight, and
// throwing that away on an interruption is the difference between a backfill that finishes
// and one that restarts forever.
func Discover(ctx context.Context, d Deps, t Target, symbols []string) ([]string, error) {
	progress, err := Status(ctx, d, t, ScopeDiscover)
	if err != nil {
		return nil, err
	}
	if progress.CompletedAt == nil {
		if err := sweep(ctx, d, t, symbols, progress.Cursor); err != nil {
			return nil, err
		}
	}
	return tradedSymbols(ctx, d, t)
}

// sweep probes each symbol after the cursor, in sorted order. Sorted, because the cursor is
// a symbol name: "everything up to here is settled" only means something if the order is
// the same on every run.
func sweep(ctx context.Context, d Deps, t Target, symbols []string, cursor string) error {
	ordered := append([]string(nil), symbols...)
	sort.Strings(ordered)

	for _, symbol := range ordered {
		if symbol <= cursor {
			continue
		}
		traded, err := hasTrades(ctx, d, symbol)
		if err != nil {
			return err
		}
		// The probe result and the cursor advance are one write, so an interruption never
		// leaves a symbol recorded as probed without the scope its trades need.
		if err := recordProbe(ctx, d, t, symbol, traded); err != nil {
			return err
		}
		cursor = symbol
	}

	completedAt := d.Now()
	return tenancy.InTx(ctx, d.DB, t.AccountID, func(q *store.Queries) error {
		return save(ctx, q, t, Progress{
			Scope: ScopeDiscover, Cursor: cursor, CompletedAt: &completedAt,
		})
	})
}

// hasTrades asks whether the account has ever filled on this symbol. One row is enough --
// the walk that follows reads the history properly, and this request only decides whether
// there is one to read.
func hasTrades(ctx context.Context, d Deps, symbol string) (bool, error) {
	page, err := d.Client.MyTrades(ctx, binance.MyTradesQuery{Symbol: symbol, Limit: 1})
	if err != nil {
		return false, fmt.Errorf("backfill: probe %s: %w", symbol, err)
	}
	rows, err := decodeRows(page, "myTrades")
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

func recordProbe(ctx context.Context, d Deps, t Target, symbol string, traded bool) error {
	return tenancy.InTx(ctx, d.DB, t.AccountID, func(q *store.Queries) error {
		if traded {
			// DO NOTHING on conflict: a sweep run again must not rewind a walk that has
			// since made progress, which would replay the whole history at weight 20 a page.
			if err := q.OpenBackfillScope(ctx, store.OpenBackfillScopeParams{
				AccountID:     t.AccountID,
				IntegrationID: t.IntegrationID,
				Scope:         ScopeTrades(symbol),
			}); err != nil {
				return fmt.Errorf("backfill: open trade scope for %s: %w", symbol, err)
			}
		}
		return save(ctx, q, t, Progress{Scope: ScopeDiscover, Cursor: symbol})
	})
}

// tradedSymbols reads back the scopes the sweep opened. Derived from the stored scopes
// rather than accumulated in memory, so a resumed sweep still reports what the interrupted
// half of it found.
func tradedSymbols(ctx context.Context, d Deps, t Target) ([]string, error) {
	scopes, err := Scopes(ctx, d, t, scopeTradesPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if symbol, ok := symbolOf(s.Scope); ok {
			out = append(out, symbol)
		}
	}
	return out, nil
}
