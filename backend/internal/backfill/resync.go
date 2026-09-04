package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
)

// minResyncSplit is the narrowest window a split will produce. Below it, a page that is
// still full means more trades landed in one second than a page holds, and continuing to
// halve would spend requests forever on a window that cannot be made to fit.
const minResyncSplit = time.Second

// ResyncWindow re-reads one time window and appends whatever it finds. It is what a
// supervisor runs after the live stream reports a gap: nothing in the protocol says what
// happened while the connection was down, so the window is read rather than assumed empty.
//
// It reads by time, never by id, and that is forced rather than chosen. rest-api.md
// enumerates the parameter combinations myTrades accepts and a time range with fromId is
// not among them, so the historical walk and the gap replay are two strategies rather than
// one with an optional parameter.
//
// Adjacent windows are passed as [a,b) and [b,c). Whether the venue treats endTime as
// inclusive is not stated anywhere, and this shape is safe either way: an inclusive endTime
// makes the boundary trade arrive twice and be deduplicated, an exclusive one makes it
// arrive once. Neither loses it, which is the only outcome that matters.
//
// It deliberately moves no cursor. The walk owns "how far back have we read"; a replay of
// ten minutes that touched that cursor would declare a symbol's whole history complete.
// Overlap is therefore free: dedup on venue identity means a generous window costs requests
// and stores nothing twice (L5).
func ResyncWindow(
	ctx context.Context, d Deps, t Target, symbols []string, from, to time.Time,
) error {
	if !from.Before(to) {
		return fmt.Errorf("backfill: resync window %s..%s is empty",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}
	if span := to.Sub(from); span > binance.MaxTradeWindow {
		return fmt.Errorf("%w: resync window spans %s, and the venue answers at most 24h",
			binance.ErrUnsupportedQuery, span)
	}

	for _, symbol := range symbols {
		if err := resyncSymbol(ctx, d, t, symbol, from, to); err != nil {
			return err
		}
	}

	events, err := readDepositWindow(ctx, d, t, from, to)
	if err != nil {
		return err
	}
	return appendOnly(ctx, d, t, events)
}

// resyncSymbol reads one symbol's fills in a window, halving the window whenever a page
// comes back full.
//
// Halving rather than paging is what the parameter table leaves us: a full page means the
// window holds at least a page of trades, and there is no supported way to ask for the rest
// of that window. Two narrower windows can be asked for, and their union is the same set.
func resyncSymbol(
	ctx context.Context, d Deps, t Target, symbol string, from, to time.Time,
) error {
	limit := d.tradeLimit()
	page, err := d.Client.MyTrades(ctx, binance.MyTradesQuery{
		Symbol: symbol, StartTime: from, EndTime: to, Limit: limit,
	})
	if err != nil {
		return fmt.Errorf("backfill: resync %s %s..%s: %w", symbol,
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), err)
	}
	rows, err := decodeRows(page, "myTrades")
	if err != nil {
		return err
	}

	if len(rows) >= limit {
		if to.Sub(from) <= minResyncSplit {
			return fmt.Errorf(
				"%w: %s put more than %d trades into one second at %s, which cannot be read"+
					" as one window",
				ErrIncomplete, symbol, limit, from.UTC().Format(time.RFC3339Nano))
		}
		middle := from.Add(to.Sub(from) / 2)
		if err := resyncSymbol(ctx, d, t, symbol, from, middle); err != nil {
			return err
		}
		return resyncSymbol(ctx, d, t, symbol, middle, to)
	}

	events, err := normalizeResyncedTrades(ctx, d, t, symbol, rows)
	if err != nil {
		return err
	}
	return appendOnly(ctx, d, t, events)
}

// normalizeResyncedTrades turns a windowed page into events. Unlike the walk it enforces no
// page contiguity: there is no cursor here to be made wrong by an out-of-order page, and the
// ids are only ever used as identities.
func normalizeResyncedTrades(
	ctx context.Context, d Deps, t Target, symbol string, rows []json.RawMessage,
) ([]ledger.Event, error) {
	ic := binance.IngestContext{
		AccountID:     t.AccountID,
		IntegrationID: t.IntegrationID,
		Source:        binance.SourceREST,
	}
	events := make([]ledger.Event, 0, len(rows))
	for _, row := range rows {
		event, err := binance.NormalizeSpotTrade(ctx, d.Registry, ic, row)
		if err != nil {
			return nil, fmt.Errorf("backfill: normalize resynced %s trade: %w", symbol, err)
		}
		events = append(events, event)
	}
	return events, nil
}

// appendOnly writes events without touching any cursor. Every other write in this package
// goes through commit, which pairs events with the cursor that describes them; this one is
// the deliberate exception, and it is deliberate because a replay describes no position in
// the walk's order.
func appendOnly(ctx context.Context, d Deps, t Target, events []ledger.Event) error {
	if len(events) == 0 {
		return nil
	}
	return tenancy.InTx(ctx, d.DB, t.AccountID, func(q *store.Queries) error {
		_, err := ledger.Append(ctx, q, events)
		return err
	})
}
