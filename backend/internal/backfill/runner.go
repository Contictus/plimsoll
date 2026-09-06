package backfill

import (
	"context"
	"time"
)

// Runner drives one integration's historical import, one chunk per call.
//
// A chunk rather than one long call, because the supervisor has to be able to stop between
// chunks when its lease is lost, and a single call that ran for an hour could not (L6).
// Each chunk is itself resumable, so stopping mid-chunk costs the chunk, never the import.
type Runner struct {
	Deps   Deps
	Target Target

	// Symbols is every spot symbol to probe, read once from exchangeInfo via
	// binance.SpotSymbols. Discovery sweeps it; nothing else uses it.
	Symbols []string

	// Since is how far back the deposit walk starts. Binance spot opened in 2017, so an
	// earlier value costs empty windows and no correctness.
	Since time.Time
}

// Step does one chunk and reports whether more history remains.
//
// The order is deposits, then discovery, then one symbol per call. Deposits first because
// they are weight 1 and give an account its balances quickly; discovery next because a
// trades scope does not exist until discovery opens it, so a runner that walked first would
// find nothing to walk and report a complete history over an account it never looked at.
//
// Once every scope is complete, Step returns false and does no further work. Deposits made
// *after* that point are not picked up here: `balanceUpdate` is not normalized yet, so an
// ongoing deposit is seen only by the next worker start or by the window a stream gap
// replays. Recorded as a known gap rather than papered over (docs/BINANCE-API-NOTES.md
// section 5).
func (r *Runner) Step(ctx context.Context) (bool, error) {
	deposits, err := Status(ctx, r.Deps, r.Target, ScopeDeposits)
	if err != nil {
		return false, err
	}
	if deposits.CompletedAt == nil {
		return true, WalkDeposits(ctx, r.Deps, r.Target, r.Since)
	}

	discover, err := Status(ctx, r.Deps, r.Target, ScopeDiscover)
	if err != nil {
		return false, err
	}
	if discover.CompletedAt == nil {
		_, err := Discover(ctx, r.Deps, r.Target, r.Symbols)
		return true, err
	}

	symbol, found, err := r.nextUnwalkedSymbol(ctx)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return true, WalkTrades(ctx, r.Deps, r.Target, symbol)
}

// nextUnwalkedSymbol returns the first symbol whose walk has not finished. First in the
// scopes' own order, which is stable, so a runner restarted mid-import picks up where the
// last one was rather than starting the sweep over.
func (r *Runner) nextUnwalkedSymbol(ctx context.Context) (string, bool, error) {
	scopes, err := Scopes(ctx, r.Deps, r.Target, scopeTradesPrefix)
	if err != nil {
		return "", false, err
	}
	for _, scope := range scopes {
		if scope.CompletedAt != nil {
			continue
		}
		if symbol, ok := SymbolOf(scope.Scope); ok {
			return symbol, true, nil
		}
	}
	return "", false, nil
}
