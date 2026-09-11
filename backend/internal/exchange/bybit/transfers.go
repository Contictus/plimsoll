package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// MaxHistoryWindow is the widest span either history endpoint answers for: "endTime -
// startTime should be less than 30 days" (B3).
//
// Under rather than equal to thirty days, because the documented word is "less than" and a
// window exactly at the limit is the boundary the venue's own wording leaves undecided. A day
// of margin costs one extra page and settles the question without asking.
const MaxHistoryWindow = 29 * 24 * time.Hour

// HistoryPageSize is the maximum rows a page returns: "[1, 50]" (B3).
const HistoryPageSize = 50

// WindowOverlap is how far each window reaches back into the one before it.
//
// The deposit page states the timestamps are milliseconds "though the query logic is actually
// effective based on second level" (B4). Which way that rounds is not stated, so a walk that
// butted its windows together could drop a row on the boundary. One second of overlap costs a
// duplicate that the dedup key discards (L5) and removes the question entirely.
const WindowOverlap = time.Second

// HistoryQuery is one page of either history endpoint.
type HistoryQuery struct {
	// Start and End bound the window. The venue requires the span to be under 30 days.
	Start, End time.Time

	// Cursor is `nextPageCursor` from the previous page. Bybit pages by cursor rather than
	// offset (B3), which is the shape that stays correct while rows are being inserted --
	// an offset walk re-reads or skips whenever the underlying set grows.
	Cursor string

	Limit int
}

// Validate refuses a window the venue will not answer for, BEFORE the request is sent.
//
// Exported because a walk builds its own windows and has to be able to check one without
// spending a request -- and because the venue answers a too-wide window with an error a walk
// could mistake for an empty page, which is a gap recorded as a complete history.
func (q HistoryQuery) Validate() error {
	if q.End.Before(q.Start) {
		return fmt.Errorf("bybit: history window ends before it starts")
	}
	if q.End.Sub(q.Start) >= 30*24*time.Hour {
		return fmt.Errorf(
			"bybit: history window is %s; the venue answers for less than 30 days (B3)",
			q.End.Sub(q.Start))
	}
	if q.Limit < 0 || q.Limit > HistoryPageSize {
		return fmt.Errorf("bybit: page size %d is outside [1, %d]", q.Limit, HistoryPageSize)
	}
	return nil
}

func (q HistoryQuery) values() url.Values {
	v := url.Values{}
	if !q.Start.IsZero() {
		v.Set("startTime", strconv.FormatInt(q.Start.UnixMilli(), 10))
	}
	if !q.End.IsZero() {
		v.Set("endTime", strconv.FormatInt(q.End.UnixMilli(), 10))
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	limit := q.Limit
	if limit == 0 {
		limit = HistoryPageSize
	}
	v.Set("limit", strconv.Itoa(limit))
	return v
}

// DepositRecords returns one page of deposit history. Signed.
func (c *Client) DepositRecords(ctx context.Context, q HistoryQuery) (json.RawMessage, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	return c.do(ctx, request{
		path: "/v5/asset/deposit/query-record", query: q.values(), cost: requestCost,
	})
}

// WithdrawRecords returns one page of withdrawal history. Signed.
//
// This endpoint exists here and has no Binance counterpart, and that asymmetry is the finding
// that shaped M8: Bybit publishes the status enum that decides whether the coins left, and
// Binance does not (B2, F5).
func (c *Client) WithdrawRecords(ctx context.Context, q HistoryQuery) (json.RawMessage, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	return c.do(ctx, request{
		path: "/v5/asset/withdraw/query-record", query: q.values(), cost: requestCost,
	})
}

// Page is the shape both history endpoints answer in: rows plus a cursor.
type Page struct {
	Rows   []json.RawMessage
	Cursor string
}

// DecodePage splits a history result into its rows and the cursor for the next one.
//
// The rows stay as raw bytes: the normalizer decodes them, and `raw` keeps them forever (L15).
func DecodePage(result json.RawMessage) (Page, error) {
	var payload struct {
		Rows           []json.RawMessage `json:"rows"`
		NextPageCursor string            `json:"nextPageCursor"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return Page{}, fmt.Errorf("bybit: decode history page: %w", err)
	}
	return Page{Rows: payload.Rows, Cursor: payload.NextPageCursor}, nil
}
