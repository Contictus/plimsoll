package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
)

// probeLimit is the page size of the two requests that check F5. Two, because the question
// is only "does this account have more than one trade, and is fromId=0 answering with the
// last of them" -- a larger page would cost the same weight and answer nothing more.
const probeLimit = 2

// WalkTrades appends one symbol's entire fill history, resuming from wherever a previous
// run stopped. It is safe to call repeatedly: a completed scope sends no requests, and a
// replayed page is deduplicated on venue identity (L5).
//
// The walk pages by trade id. That rests on an inference the documentation does not state
// -- what fromId=0 returns with no time range -- so the inference is checked at runtime
// before the first page rather than assumed (docs/BINANCE-API-NOTES.md section 5). The
// alternative, a 24-hour time walk from spot's 2017 launch, is roughly 3,300 requests per
// symbol at weight 20 and is not a backfill anyone runs.
func WalkTrades(ctx context.Context, d Deps, t Target, symbol string) error {
	scope := ScopeTrades(symbol)
	progress, err := Status(ctx, d, t, scope)
	if err != nil {
		return err
	}
	if progress.CompletedAt != nil {
		return nil
	}

	from, err := resumeFrom(progress, symbol)
	if err != nil {
		return err
	}
	if progress.Cursor == "" {
		// Only the very first page is ambiguous. Once a cursor exists, fromId carries a
		// real trade id and its meaning is documented.
		if err := verifyOldestFirst(ctx, d, symbol); err != nil {
			return err
		}
	}

	limit := d.tradeLimit()
	cursor := progress.Cursor
	for {
		page, err := d.Client.MyTrades(ctx, binance.MyTradesQuery{
			Symbol: symbol, FromID: &from, Limit: limit,
		})
		if err != nil {
			return fmt.Errorf("backfill: myTrades %s from %d: %w", symbol, from, err)
		}
		rows, err := decodeRows(page, "myTrades")
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}

		events, last, err := normalizeTradePage(ctx, d, t, symbol, rows, from)
		if err != nil {
			return err
		}
		cursor = strconv.FormatInt(last, 10)
		if err := commit(ctx, d, t, events, Progress{Scope: scope, Cursor: cursor}); err != nil {
			return err
		}

		if len(rows) < limit {
			break
		}
		from = last + 1
	}

	completedAt := d.Now()
	return commit(ctx, d, t, nil, Progress{
		Scope: scope, Cursor: cursor, CompletedAt: &completedAt,
	})
}

// resumeFrom turns a stored cursor into the next fromId. An unparsable cursor is an error
// rather than a restart: restarting silently re-reads a history that may be years long,
// and doing so on every run looks exactly like a backfill that never finishes.
func resumeFrom(progress Progress, symbol string) (int64, error) {
	if progress.Cursor == "" {
		return 0, nil
	}
	last, err := strconv.ParseInt(progress.Cursor, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("backfill: %s cursor %q is not a trade id: %w",
			symbol, progress.Cursor, err)
	}
	return last + 1, nil
}

// normalizeTradePage turns one page into events and reports the highest id it contained.
//
// It is also where page contiguity is enforced: every row must sit at or after the id the
// page was requested from, and the ids must strictly ascend. A page that starts before its
// fromId means the walk is not advancing through the history the way it believes, and
// continuing would produce a cursor describing events that were never read.
func normalizeTradePage(
	ctx context.Context, d Deps, t Target, symbol string, rows []json.RawMessage, from int64,
) ([]ledger.Event, int64, error) {
	ic := binance.IngestContext{
		AccountID:     t.AccountID,
		IntegrationID: t.IntegrationID,
		Source:        binance.SourceREST,
	}

	events := make([]ledger.Event, 0, len(rows))
	previous := from - 1
	for _, row := range rows {
		id, err := tradeID(row)
		if err != nil {
			return nil, 0, err
		}
		if id <= previous {
			return nil, 0, fmt.Errorf(
				"%w: %s page requested from %d returned id %d after %d, so the walk cannot"+
					" tell which trades it has read",
				ErrIncomplete, symbol, from, id, previous)
		}
		previous = id

		event, err := binance.NormalizeSpotTrade(ctx, d.Registry, ic, row)
		if err != nil {
			return nil, 0, fmt.Errorf("backfill: normalize %s trade %d: %w", symbol, id, err)
		}
		events = append(events, event)
	}
	return events, previous, nil
}

// verifyOldestFirst checks the one inference the trade walk rests on: that fromId=0 returns
// the oldest trades rather than the newest. Two requests, once per symbol per fresh walk.
//
// If fromId=0 answered with the newest page, a walk would read it, find nothing after it,
// and record a complete history missing everything before -- silently, with plausible
// numbers. That is the failure this project exists to make impossible, so the walk stops
// and says so instead (L11).
func verifyOldestFirst(ctx context.Context, d Deps, symbol string) error {
	newest, err := maxTradeID(ctx, d, binance.MyTradesQuery{Symbol: symbol, Limit: probeLimit})
	if err != nil {
		return err
	}
	if newest.rows < probeLimit {
		// Fewer trades than one probe page: whatever fromId=0 means, a single page holds
		// the whole history and there is nothing for it to hide.
		return nil
	}

	zero := int64(0)
	oldest, err := maxTradeID(ctx, d, binance.MyTradesQuery{
		Symbol: symbol, FromID: &zero, Limit: 1,
	})
	if err != nil {
		return err
	}
	if oldest.rows == 0 {
		return fmt.Errorf("%w: %s reports trades but fromId=0 returns none", ErrIncomplete, symbol)
	}
	if oldest.id == newest.id {
		return fmt.Errorf(
			"%w: %s answered fromId=0 with its most recent trade (%d), so paging forward"+
				" from it would skip the history before it",
			ErrIncomplete, symbol, oldest.id)
	}
	return nil
}

type probeResult struct {
	id   int64
	rows int
}

// maxTradeID reads one probe page and reports its highest id. Highest rather than last,
// so the check does not quietly depend on the order Binance chose to return them in.
func maxTradeID(ctx context.Context, d Deps, q binance.MyTradesQuery) (probeResult, error) {
	page, err := d.Client.MyTrades(ctx, q)
	if err != nil {
		return probeResult{}, fmt.Errorf("backfill: probe %s: %w", q.Symbol, err)
	}
	rows, err := decodeRows(page, "myTrades")
	if err != nil {
		return probeResult{}, err
	}

	result := probeResult{rows: len(rows)}
	for _, row := range rows {
		id, err := tradeID(row)
		if err != nil {
			return probeResult{}, err
		}
		if id > result.id {
			result.id = id
		}
	}
	return result, nil
}

// tradeID reads the venue's trade id, which is the cursor. It is read here as well as in
// the normalizer because the walk has to order pages before it knows whether the rows in
// them are storable.
func tradeID(row json.RawMessage) (int64, error) {
	var probe struct {
		ID *int64 `json:"id"`
	}
	if err := json.Unmarshal(row, &probe); err != nil || probe.ID == nil {
		return 0, fmt.Errorf("%w: a trade row with no id has no place in the walk's order",
			binance.ErrMalformedTrade)
	}
	return *probe.ID, nil
}
