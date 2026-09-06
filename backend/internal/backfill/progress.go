// Package backfill walks an exchange's history into the ledger and remembers where each
// walk got to, so an interruption resumes instead of starting over.
//
// It holds a database handle and an exchange client, which the calculation engines may not
// (L4): this is orchestration, not calculation. Nothing here decides what an event means --
// that is the normalizer's job -- and nothing here folds one, which is the projector's.
//
// The single invariant the whole package is built around: a page of events and the cursor
// that describes it are written in one transaction. Two transactions would leave a crash
// between them either losing events or replaying them forever, and the ledger is
// append-only, so neither is repairable by an UPDATE (L2).
package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The walk scopes. Each has its own cursor because the walks have different shapes: trades
// page by trade id, deposits by time window, discovery by symbol. One cursor per
// integration would have to mean all three, and finishing one walk would mark the others
// done -- a scope wrongly marked complete is never walked again, and the hole is permanent.
const (
	ScopeDiscover = "discover"
	ScopeDeposits = "deposits"

	scopeTradesPrefix = "trades:"
)

// ScopeTrades names the walk of one symbol's fills. Exported for the same reason
// binance.SpotTradeID is: a second spelling of a key is a second cursor, and a second
// cursor means the history is walked twice or not at all.
func ScopeTrades(symbol string) string { return scopeTradesPrefix + symbol }

// ReasonIncomplete is the freshness code a caller reports when a walk could not establish
// that it read the whole history (L11, K23). Degraded and visible beats confident and wrong.
const ReasonIncomplete = "backfill_incomplete"

// ErrIncomplete means the walk stopped because it could not prove it was reading from the
// beginning. It is not a transport failure -- a retry will reproduce it -- so the caller
// raises ReasonIncomplete rather than backing off.
var ErrIncomplete = errors.New("backfill: " + ReasonIncomplete)

// Client is the slice of the exchange adapter a backfill needs. An interface rather than
// *binance.Client so the walks can be tested against an account that exists only in memory:
// the questions these walks answer are about paging and resumption, and those need a fake
// exchange, not a real one.
type Client interface {
	MyTrades(ctx context.Context, q binance.MyTradesQuery) (json.RawMessage, error)
	DepositHistory(ctx context.Context, q binance.HistoryQuery) (json.RawMessage, error)
}

// Registry resolves exchange symbols and coin tickers to canonical ids, always as of the
// event's own event_time (L8). Both halves are needed because one walk moves pairs and the
// other moves single assets.
type Registry interface {
	binance.InstrumentResolver
	binance.AssetResolver
}

// Deps is everything a walk needs and nothing it can reach around. Now is a field rather
// than a call to time.Now so that a window boundary is testable without waiting for one.
type Deps struct {
	DB       tenancy.Beginner
	Client   Client
	Registry Registry
	Now      func() time.Time

	// TradePageLimit and DepositPageLimit are the per-request row caps. Zero takes the
	// documented maximum of 1000 for both; tests set them small so a handful of rows still
	// spans several pages.
	TradePageLimit   int
	DepositPageLimit int
}

// maxPageLimit is the cap both endpoints document (docs/BINANCE-API-NOTES.md section 2).
const maxPageLimit = 1000

func (d Deps) tradeLimit() int   { return pageLimit(d.TradePageLimit) }
func (d Deps) depositLimit() int { return pageLimit(d.DepositPageLimit) }

func pageLimit(configured int) int {
	if configured <= 0 || configured > maxPageLimit {
		return maxPageLimit
	}
	return configured
}

// Target is whose history is being walked. Both ids travel together because every tenant
// query carries the account (L12) and because ledger_events' composite foreign key makes a
// mismatched pair unstorable rather than merely invisible.
type Target struct {
	AccountID     uuid.UUID
	IntegrationID uuid.UUID
}

// Progress is one resumable walk. A zero Progress means the walk has never run, which is
// deliberately the same shape as "has run and got nowhere": both resume from the start.
type Progress struct {
	Scope  string
	Cursor string

	// CompletedAt is nil while the walk is unfinished. This is the field freshness reads:
	// a nil here on any scope is what turns a portfolio response into ReasonIncomplete
	// rather than into a confident total (L11).
	CompletedAt *time.Time
}

// Status reads one scope's progress. A scope that has never been written is not an error:
// it is a walk that has not started, and the zero Progress says so.
func Status(ctx context.Context, d Deps, t Target, scope string) (Progress, error) {
	var p Progress
	err := tenancy.InTx(ctx, d.DB, t.AccountID, func(q *store.Queries) error {
		row, err := q.GetBackfillProgress(ctx, store.GetBackfillProgressParams{
			AccountID: t.AccountID, IntegrationID: t.IntegrationID, Scope: scope,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			p = Progress{Scope: scope}
			return nil
		}
		if err != nil {
			return fmt.Errorf("backfill: read progress %s: %w", scope, err)
		}
		p = Progress{Scope: row.Scope, Cursor: row.Cursor, CompletedAt: row.CompletedAt}
		return nil
	})
	return p, err
}

// Scopes lists every walk whose name starts with prefix, in a stable order. Freshness
// reporting reads it to answer "is this account's history whole?" without having to know
// which symbols exist.
func Scopes(ctx context.Context, d Deps, t Target, prefix string) ([]Progress, error) {
	var out []Progress
	err := tenancy.InTx(ctx, d.DB, t.AccountID, func(q *store.Queries) error {
		rows, err := q.ListBackfillProgress(ctx, store.ListBackfillProgressParams{
			AccountID: t.AccountID, IntegrationID: t.IntegrationID,
			ScopePrefix: prefix + "%",
		})
		if err != nil {
			return fmt.Errorf("backfill: list progress %q: %w", prefix, err)
		}
		out = make([]Progress, 0, len(rows))
		for _, row := range rows {
			out = append(out, Progress{
				Scope: row.Scope, Cursor: row.Cursor, CompletedAt: row.CompletedAt,
			})
		}
		return nil
	})
	return out, err
}

// commit is the one write path in this package: the events a page produced and the cursor
// that describes them, in a single transaction. Passing no events is normal -- a window
// with nothing in it still advances the cursor, and a completed walk records only that.
func commit(ctx context.Context, d Deps, t Target, events []ledger.Event, p Progress) error {
	return tenancy.InTx(ctx, d.DB, t.AccountID, func(q *store.Queries) error {
		if _, err := ledger.Append(ctx, q, events); err != nil {
			return err
		}
		return save(ctx, q, t, p)
	})
}

func save(ctx context.Context, q *store.Queries, t Target, p Progress) error {
	if err := q.UpsertBackfillProgress(ctx, store.UpsertBackfillProgressParams{
		AccountID:     t.AccountID,
		IntegrationID: t.IntegrationID,
		Scope:         p.Scope,
		Cursor:        p.Cursor,
		CompletedAt:   p.CompletedAt,
	}); err != nil {
		return fmt.Errorf("backfill: save progress %s at %q: %w", p.Scope, p.Cursor, err)
	}
	return nil
}

// decodeRows splits a top-level JSON array into its elements without decoding them. The
// elements stay json.RawMessage all the way to the normalizer and into the ledger's raw
// column, so no number is ever rewritten on the way through (L1, L15).
func decodeRows(payload json.RawMessage, what string) ([]json.RawMessage, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal(payload, &rows); err != nil {
		return nil, fmt.Errorf("backfill: %s response is not an array: %w", what, err)
	}
	return rows, nil
}

// SymbolOf recovers the symbol from a trades scope: the reverse of ScopeTrades. Exported
// for the same reason ScopeTrades is -- a second place that knows how a scope is spelled is
// a second place for the two to disagree.
func SymbolOf(scope string) (string, bool) {
	return strings.CutPrefix(scope, scopeTradesPrefix)
}
