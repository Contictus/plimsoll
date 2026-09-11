// The Bybit history walks. Separate from the Binance ones because the two venues page
// differently in every respect that matters: thirty-day windows against ninety, fifty rows
// against a thousand, and a cursor against an offset (B3).

package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/bybit"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
)

// isUnsettled reports whether the normalizer refused a row because the movement has not
// completed, which is the one refusal a walk continues past.
func isUnsettled(err error) bool { return errors.Is(err, bybit.ErrNotSettled) }

// BybitSource is the slice of the Bybit client a walk needs. An interface so the walk is
// tested against a venue that can be made to page, fail and repeat on demand -- a history
// walk developed against the live API can only be exercised when it is already broken.
type BybitSource interface {
	DepositRecords(ctx context.Context, q bybit.HistoryQuery) (json.RawMessage, error)
	WithdrawRecords(ctx context.Context, q bybit.HistoryQuery) (json.RawMessage, error)
}

// BybitDeps is one Bybit integration's walk, assembled.
type BybitDeps struct {
	DB       tenancy.Beginner
	Source   BybitSource
	Registry Registry
	Now      func() time.Time

	// PageLimit is the rows per request. Zero takes the venue's documented maximum of 50;
	// tests set it small so a handful of rows still spans several pages.
	PageLimit int
}

func (d BybitDeps) pageLimit() int {
	if d.PageLimit <= 0 || d.PageLimit > bybit.HistoryPageSize {
		return bybit.HistoryPageSize
	}
	return d.PageLimit
}

func (d BybitDeps) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now().UTC()
}

// WalkBybitDeposits and WalkBybitWithdrawals append every settled movement between since and
// now, in windows the venue will answer for, resuming from the last boundary reached.
//
// Both directions exist here, which they do not for Binance. That asymmetry is the whole of
// B2: Bybit publishes the status enum that decides whether the coins moved, and Binance
// publishes a garbled fragment -- so one venue's withdrawals can be recorded and the other's
// cannot, and the difference is documentation rather than effort.
func WalkBybitDeposits(ctx context.Context, d BybitDeps, t Target, since time.Time) error {
	return walkBybitHistory(ctx, d, t, since, ScopeDeposits, d.Source.DepositRecords,
		bybit.NormalizeDeposit)
}

// WalkBybitWithdrawals is the outbound half, on the same shape.
func WalkBybitWithdrawals(ctx context.Context, d BybitDeps, t Target, since time.Time) error {
	return walkBybitHistory(ctx, d, t, since, ScopeWithdrawals, d.Source.WithdrawRecords,
		bybit.NormalizeWithdrawal)
}

type bybitReader func(ctx context.Context, q bybit.HistoryQuery) (json.RawMessage, error)

type bybitNormalizer func(
	ctx context.Context, r bybit.AssetResolver, ic bybit.IngestContext, raw json.RawMessage,
) (ledger.Event, error)

// walkBybitHistory is the shape both directions share.
//
// The cursor stored between runs is the WINDOW BOUNDARY, not the venue's page cursor. A page
// cursor is only meaningful inside the window it was issued for, so persisting one would
// resume a walk into a window that no longer exists; the boundary is a time, and a time means
// the same thing tomorrow.
func walkBybitHistory(
	ctx context.Context, d BybitDeps, t Target, since time.Time,
	scope string, read bybitReader, normalize bybitNormalizer,
) error {
	progress, err := statusFor(ctx, d.DB, t, scope)
	if err != nil {
		return err
	}
	start, err := resumeAt(progress, since)
	if err != nil {
		return err
	}
	end := d.now()

	cursor := start
	for cursor.Before(end) {
		windowEnd := cursor.Add(bybit.MaxHistoryWindow)
		if windowEnd.After(end) {
			windowEnd = end
		}
		events, err := readBybitWindow(ctx, d, t, cursor, windowEnd, read, normalize)
		if err != nil {
			return err
		}
		if err := commitTo(ctx, d.DB, t, events, Progress{
			Scope: scope, Cursor: windowEnd.Format(cursorLayout),
		}); err != nil {
			return err
		}
		// The last window is the last: once it reaches `end` there is nothing after it, and
		// the overlap below would otherwise step back one second, find itself before `end`
		// again, and walk the same final window forever.
		if !windowEnd.Before(end) {
			break
		}
		// Every other window starts one second BEFORE the previous ended. The venue documents
		// its query logic as second-level even though the parameters are milliseconds (B4),
		// and does not say which way it rounds -- so butted windows could drop a row sitting
		// on a boundary. The overlap costs a duplicate the dedup key discards (L5).
		cursor = windowEnd.Add(-bybit.WindowOverlap)
	}

	completedAt := d.now()
	return commitTo(ctx, d.DB, t, nil, Progress{
		Scope:       scope,
		Cursor:      end.Format(cursorLayout),
		CompletedAt: &completedAt,
	})
}

// readBybitWindow pages one pinned window by cursor until the venue stops offering one, and
// returns the events it produced. The window is read whole before anything is committed, so
// the boundary never advances past a page that was not written.
func readBybitWindow(
	ctx context.Context, d BybitDeps, t Target, start, end time.Time,
	read bybitReader, normalize bybitNormalizer,
) ([]ledger.Event, error) {
	ic := bybit.IngestContext{
		AccountID:     t.AccountID,
		IntegrationID: t.IntegrationID,
		Source:        "rest",
	}

	var out []ledger.Event
	pageCursor := ""
	for {
		result, err := read(ctx, bybit.HistoryQuery{
			Start: start, End: end, Cursor: pageCursor, Limit: d.pageLimit(),
		})
		if err != nil {
			return nil, fmt.Errorf("backfill: bybit history %s..%s: %w",
				start.Format(time.RFC3339), end.Format(time.RFC3339), err)
		}
		page, err := bybit.DecodePage(result)
		if err != nil {
			return nil, err
		}

		for _, row := range page.Rows {
			event, err := normalize(ctx, d.Registry, ic, row)
			if err != nil {
				// A movement that has not settled is not an error in the walk: it is an
				// answer about something that has not happened, and it will be settled --
				// or not -- by the time this window is walked again.
				if isUnsettled(err) {
					continue
				}
				return nil, err
			}
			out = append(out, event)
		}

		// An empty cursor means there is no next page (B3). A cursor identical to the one we
		// sent would page forever, which is the failure a cursor-paged walk has and an
		// offset-paged one does not.
		if page.Cursor == "" || page.Cursor == pageCursor {
			return out, nil
		}
		pageCursor = page.Cursor
	}
}
