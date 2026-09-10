package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
)

// The futures scopes. One per symbol for fills, and a single one for income: the income
// endpoint takes no symbol and returns every cash flow at once, so splitting it per symbol
// would multiply the most expensive request in this system by the size of the book.
const (
	scopeFuturesTradesPrefix = "usdm:trades:"

	// ScopeIncome is the funding walk, and the discovery that opens the fill walks.
	ScopeIncome = "usdm:income"
)

// ScopeFuturesTrades names the walk of one perpetual's fills.
func ScopeFuturesTrades(symbol string) string { return scopeFuturesTradesPrefix + symbol }

// FuturesSymbolOf reads the symbol back out of a futures trade scope.
func FuturesSymbolOf(scope string) (string, bool) {
	prefix := scopeFuturesTradesPrefix
	if len(scope) <= len(prefix) || scope[:len(prefix)] != prefix {
		return "", false
	}
	return scope[len(prefix):], true
}

// futuresLimit is the page size both endpoints share.
func (d Deps) futuresLimit() int {
	if d.FuturesPageLimit > 0 && d.FuturesPageLimit < binance.FuturesPageSize {
		return d.FuturesPageLimit
	}
	return binance.FuturesPageSize
}

// futuresRange is the window a walk covers: the venue's three-month horizon, resumed from
// wherever the cursor reached. An unparsable cursor restarts the horizon rather than failing
// -- three months is thirteen requests, and re-reading them is cheaper than a walk that
// refuses to run.
func futuresRange(d Deps, progress Progress) (start, end time.Time) {
	end = d.Now().UTC()
	start = end.Add(-binance.FuturesHistory)
	if progress.Cursor == "" {
		return start, end
	}
	resumed, err := time.Parse(time.RFC3339, progress.Cursor)
	if err != nil || resumed.Before(start) {
		return start, end
	}
	return resumed, end
}

// WalkIncome appends the account's futures cash flows and opens a trade scope for every
// symbol it sees.
//
// It runs before the fill walk on purpose. The income history is the cheapest way to learn
// WHICH perpetuals this account has touched -- one walk over three months, no symbol
// required -- and sweeping every listed contract instead would be several hundred symbols
// walked to find the four the account trades.
func WalkIncome(ctx context.Context, d Deps, t Target) error {
	progress, err := Status(ctx, d, t, ScopeIncome)
	if err != nil {
		return err
	}
	if progress.CompletedAt != nil {
		return nil
	}

	start, end := futuresRange(d, progress)
	for from := start; from.Before(end); from = from.Add(binance.FuturesWindow) {
		to := from.Add(binance.FuturesWindow)
		if to.After(end) {
			to = end
		}
		if err := readIncomeWindow(ctx, d, t, from, to); err != nil {
			return err
		}
	}

	completedAt := d.Now()
	return commit(ctx, d, t, nil, Progress{
		Scope: ScopeIncome, Cursor: end.UTC().Format(time.RFC3339), CompletedAt: &completedAt,
	})
}

// readIncomeWindow reads one window, page by page.
func readIncomeWindow(ctx context.Context, d Deps, t Target, from, to time.Time) error {
	limit := d.futuresLimit()
	for page := 1; ; page++ {
		body, err := d.Client.IncomeHistory(ctx, binance.IncomeQuery{
			StartTime: from, EndTime: to, Page: page, Limit: limit,
		})
		if err != nil {
			return fmt.Errorf("backfill: income %s..%s page %d: %w",
				from.Format(time.RFC3339), to.Format(time.RFC3339), page, err)
		}
		rows, err := decodeRows(body, "income")
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}

		events, symbols, err := normalizeIncomePage(ctx, d, t, rows)
		if err != nil {
			return err
		}
		if err := commit(ctx, d, t, events, Progress{
			Scope: ScopeIncome, Cursor: to.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
		// Opened after the events are committed, so a symbol is only announced as traded
		// once there is a reason in the ledger to believe it.
		if err := openTradeScopes(ctx, d, t, symbols); err != nil {
			return err
		}

		if len(rows) < limit {
			return nil
		}
	}
}

// normalizeIncomePage folds what it can and skips what it must, by name.
//
// The three deliberate skips -- transfers, realized PnL and commission -- are not errors:
// each is a second copy of something already ingested elsewhere, and folding it would count
// that thing twice (F12, K5, L9). An UNRECOGNIZED type is a different matter and stops the
// walk, because an unknown cash flow silently folded as nothing is a balance that drifts by
// exactly the amount nobody looked at.
func normalizeIncomePage(
	ctx context.Context, d Deps, t Target, rows []json.RawMessage,
) ([]ledger.Event, []string, error) {
	ic := binance.IngestContext{
		AccountID:     t.AccountID,
		IntegrationID: t.IntegrationID,
		Source:        binance.SourceREST,
	}

	events := make([]ledger.Event, 0, len(rows))
	seen := map[string]bool{}
	symbols := make([]string, 0)
	for _, row := range rows {
		if symbol, ok := symbolOfRow(row); ok && !seen[symbol] {
			seen[symbol] = true
			symbols = append(symbols, symbol)
		}
		event, err := binance.NormalizeIncome(ctx, d.Registry, ic, row)
		if errors.Is(err, binance.ErrIncomeReportedElsewhere) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("backfill: normalize income: %w", err)
		}
		events = append(events, event)
	}
	return events, symbols, nil
}

// symbolOfRow reads just the symbol, without committing to the rest of the row's shape: a
// row this build refuses to fold still tells us which contract the account has touched.
func symbolOfRow(row json.RawMessage) (string, bool) {
	var envelope struct {
		Symbol string `json:"symbol"`
	}
	if err := json.Unmarshal(row, &envelope); err != nil || envelope.Symbol == "" {
		return "", false
	}
	return envelope.Symbol, true
}

// openTradeScopes records that a symbol is worth walking.
//
// An existing walk is left exactly as it is -- cursor and all. Writing an empty Progress over
// a walk in flight would reset its cursor to the start of the horizon, and every income page
// would restart a fill walk that was halfway through. Status cannot answer "does the row
// exist" (it returns the scope name either way, by design), so existence is read off the
// cursor and the completion together.
func openTradeScopes(ctx context.Context, d Deps, t Target, symbols []string) error {
	walked, err := Scopes(ctx, d, t, scopeFuturesTradesPrefix)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(walked))
	for _, w := range walked {
		known[w.Scope] = true
	}

	for _, symbol := range symbols {
		scope := ScopeFuturesTrades(symbol)
		if known[scope] {
			continue
		}
		if err := commit(ctx, d, t, nil, Progress{Scope: scope}); err != nil {
			return err
		}
		known[scope] = true
	}
	return nil
}

// WalkFuturesTrades appends one perpetual's fill history over the venue's three-month
// horizon, resuming from the window a previous run reached.
//
// Time-windowed rather than id-paged, unlike spot. Spot pages by trade id because its
// history reaches back to 2017; here the venue answers for three months at most, so thirteen
// windows cover the whole of what exists and nothing has to be inferred about what fromId=0
// means (F5).
func WalkFuturesTrades(ctx context.Context, d Deps, t Target, symbol string) error {
	scope := ScopeFuturesTrades(symbol)
	progress, err := Status(ctx, d, t, scope)
	if err != nil {
		return err
	}
	if progress.CompletedAt != nil {
		return nil
	}

	ic := binance.IngestContext{
		AccountID: t.AccountID, IntegrationID: t.IntegrationID, Source: binance.SourceREST,
	}
	start, end := futuresRange(d, progress)
	limit := d.futuresLimit()

	for from := start; from.Before(end); from = from.Add(binance.FuturesWindow) {
		to := from.Add(binance.FuturesWindow)
		if to.After(end) {
			to = end
		}

		body, err := d.Client.FuturesUserTrades(ctx, binance.FuturesTradesQuery{
			Symbol: symbol, StartTime: from, EndTime: to, Limit: limit,
		})
		if err != nil {
			return fmt.Errorf("backfill: userTrades %s %s: %w",
				symbol, from.Format(time.RFC3339), err)
		}
		rows, err := decodeRows(body, "userTrades")
		if err != nil {
			return err
		}

		events := make([]ledger.Event, 0, len(rows))
		for _, row := range rows {
			event, err := binance.NormalizeFuturesTrade(ctx, d.Registry, ic, row)
			if err != nil {
				return fmt.Errorf("backfill: normalize %s fill: %w", symbol, err)
			}
			events = append(events, event)
		}
		if err := commit(ctx, d, t, events, Progress{
			Scope: scope, Cursor: to.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}

		if len(rows) == limit {
			// A full page inside a seven-day window means the window itself was truncated,
			// and this endpoint has no page parameter to ask for the rest of it. Reported
			// rather than passed over: the missing fills would be a hole in a position's
			// history that nothing ever revisits (L11).
			return fmt.Errorf("%w: %s filled a whole page between %s and %s",
				ErrIncomplete, symbol, from.Format(time.RFC3339), to.Format(time.RFC3339))
		}
	}

	completedAt := d.Now()
	return commit(ctx, d, t, nil, Progress{
		Scope: scope, Cursor: end.UTC().Format(time.RFC3339), CompletedAt: &completedAt,
	})
}

// ResyncFuturesSymbol re-reads one perpetual's fills over a bounded window and appends
// whatever it finds.
//
// This is what the live stream triggers. The stream says which contract moved and when; this
// says what happened -- because the identity of a fill has to be the one the walk mints (L5),
// and two spellings of a key are a doubled position. The stream is a latency improvement
// that cannot invent a number.
//
// It touches no cursor: a resync is not progress through history, and letting it write one
// would let a five-minute window mark a three-month walk complete.
func ResyncFuturesSymbol(
	ctx context.Context, d Deps, t Target, symbol string, from, to time.Time,
) error {
	if !from.Before(to) {
		return nil
	}
	if window := to.Sub(from); window > binance.FuturesWindow {
		// The venue rejects a wider range outright, so a caller asking for one gets an
		// answer rather than a request that fails at the edge.
		return fmt.Errorf("backfill: futures resync window %s exceeds the venue's %s limit",
			window, binance.FuturesWindow)
	}

	body, err := d.Client.FuturesUserTrades(ctx, binance.FuturesTradesQuery{
		Symbol: symbol, StartTime: from, EndTime: to, Limit: d.futuresLimit(),
	})
	if err != nil {
		return fmt.Errorf("backfill: resync %s: %w", symbol, err)
	}
	rows, err := decodeRows(body, "userTrades")
	if err != nil {
		return err
	}

	ic := binance.IngestContext{
		AccountID: t.AccountID, IntegrationID: t.IntegrationID, Source: binance.SourceREST,
	}
	events := make([]ledger.Event, 0, len(rows))
	for _, row := range rows {
		event, err := binance.NormalizeFuturesTrade(ctx, d.Registry, ic, row)
		if err != nil {
			return fmt.Errorf("backfill: normalize %s fill: %w", symbol, err)
		}
		events = append(events, event)
	}
	return commit(ctx, d, t, events, Progress{})
}
