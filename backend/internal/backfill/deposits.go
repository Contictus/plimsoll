package backfill

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
)

// depositWindow is the span the deposit-history endpoint documents as its maximum
// (docs/BINANCE-API-NOTES.md section 2). A wider window is rejected by Binance; a narrower
// one is accepted and merely slow, so only one of the two mistakes announces itself, which
// is why the number lives here as a constant rather than at a call site.
const depositWindow = 90 * 24 * time.Hour

// cursorLayout is how a window boundary is stored. RFC 3339 with nanoseconds, in UTC, so
// the cursor round-trips to the same instant and sorts as text in the order it happened.
const cursorLayout = time.RFC3339Nano

// WalkDeposits appends every settled deposit between since and now, in 90-day windows,
// resuming from the last window boundary it reached.
//
// Unlike the trade walk it does not stop at a completed scope: deposits keep arriving, so
// "complete" means "walked up to the cursor", and a later run continues from there. The
// cursor is the boundary rather than a record id because the endpoint pages by offset, and
// offset paging over a window that is still moving is exactly what skips and repeats rows.
// Pinning the boundary by time is what makes the offset within it safe.
//
// Withdrawals have no walk here. Their status enum and the timezone of applyTime are both
// undocumented, so NormalizeWithdrawal does not exist, and encoding a remembered enum into
// append-only financial rows is what CLAUDE.md section 2 forbids.
func WalkDeposits(ctx context.Context, d Deps, t Target, since time.Time) error {
	progress, err := Status(ctx, d, t, ScopeDeposits)
	if err != nil {
		return err
	}

	start, err := resumeAt(progress, since)
	if err != nil {
		return err
	}
	end := d.Now().UTC()

	cursor := start
	for cursor.Before(end) {
		windowEnd := cursor.Add(depositWindow)
		if windowEnd.After(end) {
			windowEnd = end
		}
		events, err := readDepositWindow(ctx, d, t, cursor, windowEnd)
		if err != nil {
			return err
		}
		if err := commit(ctx, d, t, events, Progress{
			Scope: ScopeDeposits, Cursor: windowEnd.Format(cursorLayout),
		}); err != nil {
			return err
		}
		cursor = windowEnd
	}

	completedAt := d.Now()
	return commit(ctx, d, t, nil, Progress{
		Scope:       ScopeDeposits,
		Cursor:      cursor.Format(cursorLayout),
		CompletedAt: &completedAt,
	})
}

// resumeAt picks the window this run starts from: the stored boundary if there is one, and
// otherwise the caller's `since`. An unparsable cursor is an error rather than a restart --
// a silent restart re-reads years of history on every run and looks like progress.
func resumeAt(progress Progress, since time.Time) (time.Time, error) {
	if progress.Cursor == "" {
		return since.UTC(), nil
	}
	at, err := time.Parse(cursorLayout, progress.Cursor)
	if err != nil {
		return time.Time{}, fmt.Errorf("backfill: deposits cursor %q is not a timestamp: %w",
			progress.Cursor, err)
	}
	return at.UTC(), nil
}

// readDepositWindow pages one pinned window by offset until it is exhausted, and returns
// the events it produced. A window is read whole before anything is committed, so the
// cursor never advances past a page that was not written.
func readDepositWindow(
	ctx context.Context, d Deps, t Target, start, end time.Time,
) ([]ledger.Event, error) {
	ic := binance.IngestContext{
		AccountID:     t.AccountID,
		IntegrationID: t.IntegrationID,
		Source:        binance.SourceREST,
	}

	limit := d.depositLimit()
	var events []ledger.Event
	for offset := 0; ; {
		page, err := d.Client.DepositHistory(ctx, binance.HistoryQuery{
			StartTime: start, EndTime: end, Offset: offset, Limit: limit,
		})
		if err != nil {
			return nil, fmt.Errorf("backfill: deposits %s..%s offset %d: %w",
				start.Format(cursorLayout), end.Format(cursorLayout), offset, err)
		}
		rows, err := decodeRows(page, "deposit history")
		if err != nil {
			return nil, err
		}

		for _, row := range rows {
			event, err := binance.NormalizeDeposit(ctx, d.Registry, ic, row)
			if errors.Is(err, binance.ErrNotSettled) {
				// Not money in the account yet. Skipping is correct and temporary: the row
				// is read again by a later run once its status changes, and the ledger is
				// append-only, so crediting it early could only be undone by a second
				// event reversing the first (L2).
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("backfill: normalize deposit: %w", err)
			}
			events = append(events, event)
		}

		if len(rows) < limit {
			return events, nil
		}
		offset += len(rows)
	}
}
