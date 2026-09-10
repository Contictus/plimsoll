package backfill

import (
	"context"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
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
// The order is deposits, then one transfer direction per call, then discovery, then one
// symbol per call. Deposits and transfers first because they are weight 1 and give an
// account its balances quickly; discovery next because a trades scope does not exist until
// discovery opens it, so a runner that walked first would find nothing to walk and report a
// complete history over an account it never looked at.
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

	// Transfers next, and one direction per chunk. They are weight 1 like deposits, and
	// they are what stops a spot -> futures move being read as a sale once the fills around
	// it arrive (M3.5).
	if transferType, found, err := r.nextUnwalkedTransferType(ctx); err != nil {
		return false, err
	} else if found {
		return true, WalkTransferDirection(ctx, r.Deps, r.Target, transferType, r.Since)
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
	if found {
		return true, WalkTrades(ctx, r.Deps, r.Target, symbol)
	}

	// The USD-M half, last: it is the most expensive per request (income is weight 30, six
	// times anything else) and the spot history is what most accounts have most of.
	//
	// Income before fills, because the income walk is what DISCOVERS which perpetuals this
	// account has touched. Sweeping every listed contract instead would be several hundred
	// symbols walked to find the four that matter.
	income, err := Status(ctx, r.Deps, r.Target, ScopeIncome)
	if err != nil {
		return false, err
	}
	if income.CompletedAt == nil {
		return true, WalkIncome(ctx, r.Deps, r.Target)
	}

	perp, found, err := r.nextUnwalkedPerp(ctx)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return true, WalkFuturesTrades(ctx, r.Deps, r.Target, perp)
}

// nextUnwalkedPerp returns the first perpetual whose fill walk has not finished. Its scopes
// are opened by the income walk, the way spot's are opened by discovery.
func (r *Runner) nextUnwalkedPerp(ctx context.Context) (string, bool, error) {
	scopes, err := Scopes(ctx, r.Deps, r.Target, scopeFuturesTradesPrefix)
	if err != nil {
		return "", false, err
	}
	for _, scope := range scopes {
		if scope.CompletedAt != nil {
			continue
		}
		if symbol, ok := FuturesSymbolOf(scope.Scope); ok {
			return symbol, true, nil
		}
	}
	return "", false, nil
}

// nextUnwalkedTransferType returns the first direction whose walk has not finished, in the
// normalizer's own order. The directions come from binance.WalkedTransferTypes rather than
// from the stored scopes, unlike the symbol walk: a symbol scope is opened by discovery, so
// it exists before it is walked, while a transfer direction is known in advance and its
// scope only exists once something has walked it.
func (r *Runner) nextUnwalkedTransferType(ctx context.Context) (string, bool, error) {
	for _, transferType := range binance.WalkedTransferTypes() {
		progress, err := Status(ctx, r.Deps, r.Target, ScopeTransfers(transferType))
		if err != nil {
			return "", false, err
		}
		if progress.CompletedAt == nil {
			return transferType, true, nil
		}
	}
	return "", false, nil
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
