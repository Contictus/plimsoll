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

// TransferHistory is how far back the universal-transfer endpoint answers at all. Quoted
// from the page on 2026-09-09: "Support query within the last 6 months only".
//
// Exported because it is a boundary on the account's history rather than an implementation
// detail: transfers older than this cannot be imported from this venue by anyone, and a
// reader who wants to know why the ledger starts where it starts needs the number.
const TransferHistory = 180 * 24 * time.Hour

// transferWindow is the span of one pinned query window.
//
// Unlike depositWindow this is NOT a documented cap -- the page states a 6-month horizon and
// no per-query limit. The window exists for the other reason: paging here is `current` and
// `size` over a table that receives new rows, so the pages shift under a reader who is not
// holding the range still (F10). Thirty days is short enough that a window rarely exceeds
// the 100-row page cap and long enough that six months is a handful of queries.
const transferWindow = 30 * 24 * time.Hour

// WalkTransfers appends every confirmed intra-venue transfer between since and now, one
// scope per direction, resuming each from the last window boundary it reached.
//
// Eight walks rather than one, because `type` is a required parameter on the endpoint and is
// a direction rather than a category: MAIN_UMFUTURE and UMFUTURE_MAIN are two queries (F10).
// Each keeps its own cursor, so an import interrupted in the sixth direction resumes there
// instead of re-reading the five that finished.
//
// The directions come from binance.WalkedTransferTypes rather than from a list here. A
// second list would be a second place for the walk and the normalizer to disagree about
// which transfers exist, and the symptom of that disagreement is transfers that are never
// fetched -- indistinguishable, downstream, from transfers that never happened.
func WalkTransfers(ctx context.Context, d Deps, t Target, since time.Time) error {
	for _, transferType := range binance.WalkedTransferTypes() {
		if err := WalkTransferDirection(ctx, d, t, transferType, since); err != nil {
			return err
		}
	}
	return nil
}

// WalkTransferDirection walks one direction. Exported because the runner does one chunk per
// Step and a chunk has to be small enough to stop between: eight directions in one call
// would hold the lease through all of them, and losing it partway would mean the work was
// done by a process that no longer had the right to do it (L6).
func WalkTransferDirection(
	ctx context.Context, d Deps, t Target, transferType string, since time.Time,
) error {
	start, end := transferRange(d, since)
	return walkOneDirection(ctx, d, t, transferType, start, end)
}

// transferRange clamps the caller's `since` to the venue's horizon.
//
// The horizon is the venue's, not ours: asking earlier returns nothing, and a walk that
// recorded those empty windows as walked would mark a scope complete over months it never
// saw.
func transferRange(d Deps, since time.Time) (start, end time.Time) {
	end = d.Now().UTC()
	earliest := end.Add(-TransferHistory)
	start = since.UTC()
	if start.Before(earliest) {
		start = earliest
	}
	return start, end
}

// walkOneDirection walks a single transfer type in pinned windows, committing each window
// before advancing its cursor.
func walkOneDirection(
	ctx context.Context, d Deps, t Target, transferType string, since, end time.Time,
) error {
	scope := ScopeTransfers(transferType)

	progress, err := Status(ctx, d, t, scope)
	if err != nil {
		return err
	}
	cursor, err := resumeTransfersAt(progress, transferType, since)
	if err != nil {
		return err
	}

	for cursor.Before(end) {
		windowEnd := cursor.Add(transferWindow)
		if windowEnd.After(end) {
			windowEnd = end
		}
		events, err := readTransferWindow(ctx, d, t, transferType, cursor, windowEnd)
		if err != nil {
			return err
		}
		if err := commit(ctx, d, t, events, Progress{
			Scope: scope, Cursor: windowEnd.Format(cursorLayout),
		}); err != nil {
			return err
		}
		cursor = windowEnd
	}

	completedAt := d.Now()
	return commit(ctx, d, t, nil, Progress{
		Scope:       scope,
		Cursor:      cursor.Format(cursorLayout),
		CompletedAt: &completedAt,
	})
}

// resumeTransfersAt picks the boundary this direction's run starts from. An unparsable
// cursor is an error rather than a restart: a silent restart re-reads six months on every
// run and looks like progress.
func resumeTransfersAt(progress Progress, transferType string, since time.Time) (time.Time, error) {
	if progress.Cursor == "" {
		return since.UTC(), nil
	}
	at, err := time.Parse(cursorLayout, progress.Cursor)
	if err != nil {
		return time.Time{}, fmt.Errorf("backfill: %s cursor %q is not a timestamp: %w",
			ScopeTransfers(transferType), progress.Cursor, err)
	}
	if at.Before(since) {
		// The stored cursor has fallen off the venue's 6-month horizon. Resuming from it
		// would spend queries on windows the endpoint answers with nothing.
		return since.UTC(), nil
	}
	return at.UTC(), nil
}

// readTransferWindow pages one pinned window until it is exhausted, and returns the events
// it produced. The window is read whole before anything is committed, so the cursor never
// advances past a page that was not written.
func readTransferWindow(
	ctx context.Context, d Deps, t Target, transferType string, start, end time.Time,
) ([]ledger.Event, error) {
	ic := binance.IngestContext{
		AccountID:     t.AccountID,
		IntegrationID: t.IntegrationID,
		Source:        binance.SourceREST,
	}

	size := d.transferPageSize()
	var events []ledger.Event
	for page := 1; ; page++ {
		body, err := d.Client.UniversalTransferHistory(ctx, binance.TransferQuery{
			Type: transferType, StartTime: start, EndTime: end, Page: page, Size: size,
		})
		if err != nil {
			return nil, fmt.Errorf("backfill: transfers %s %s..%s page %d: %w",
				transferType, start.Format(cursorLayout), end.Format(cursorLayout), page, err)
		}
		rows, err := decodeTransferPage(body)
		if err != nil {
			return nil, err
		}

		for _, row := range rows {
			event, err := binance.NormalizeTransfer(ctx, d.Registry, ic, row)
			if errors.Is(err, binance.ErrTransferNotConfirmed) {
				// Not a movement that has happened. Skipping is correct and temporary: the
				// row is read again by a later run once its status changes, and the ledger
				// is append-only, so recording it early could only be undone by a second
				// event reversing the first (L2).
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("backfill: normalize transfer: %w", err)
			}
			events = append(events, event)
		}

		if len(rows) < size {
			return events, nil
		}
	}
}

// transferPage is the endpoint's response shape: an object with a count and the rows, not
// the bare array the deposit endpoints return.
type transferPage struct {
	Total int               `json:"total"`
	Rows  []json.RawMessage `json:"rows"`
}

// decodeTransferPage reads one page's rows.
//
// `total` is deliberately ignored. It counts the whole window rather than the page, and it
// is computed by the venue at query time -- trusting it to decide when to stop would make
// the walk's termination depend on a number that can change between two pages of the same
// window. The page's own length against the requested size is a fact about the response in
// hand.
func decodeTransferPage(body json.RawMessage) ([]json.RawMessage, error) {
	var page transferPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("backfill: decode transfer history: %w", err)
	}
	return page.Rows, nil
}

// ScopeTransfers names the walk of one transfer direction. Exported for the same reason
// ScopeTrades is: the runner reads these scopes back to decide what is left to do, and a
// second spelling would make a walked direction look unwalked forever.
func ScopeTransfers(transferType string) string {
	return scopeTransfersPrefix + transferType
}
