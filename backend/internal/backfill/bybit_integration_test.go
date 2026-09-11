//go:build integration

package backfill_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/backfill"
	"github.com/Contictus/plimsoll/backend/internal/exchange/bybit"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/stretchr/testify/require"
)

// fakeBybit is a venue that answers with the rows a test gave it, paging by cursor the way
// Bybit does, and recording every window it was asked for so the walk's own arithmetic can
// be asserted rather than assumed.
type fakeBybit struct {
	deposits    []json.RawMessage
	withdrawals []json.RawMessage

	windows  [][2]time.Time
	pageSize int
	calls    int
}

func (f *fakeBybit) DepositRecords(_ context.Context, q bybit.HistoryQuery) (json.RawMessage, error) {
	return f.page(q, f.deposits)
}

func (f *fakeBybit) WithdrawRecords(_ context.Context, q bybit.HistoryQuery) (json.RawMessage, error) {
	return f.page(q, f.withdrawals)
}

// page serves one cursor-paged slice, and refuses a window the venue itself would refuse --
// so a walk that built a 30-day window fails here rather than passing against a fake that was
// more forgiving than the venue.
func (f *fakeBybit) page(q bybit.HistoryQuery, rows []json.RawMessage) (json.RawMessage, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	f.calls++
	if q.Cursor == "" {
		f.windows = append(f.windows, [2]time.Time{q.Start, q.End})
	}

	// Only the rows whose instant falls inside the window, which is what makes the walk's
	// window arithmetic observable.
	var inWindow []json.RawMessage
	for _, row := range rows {
		at := instantOf(row)
		if !at.Before(q.Start) && at.Before(q.End) {
			inWindow = append(inWindow, row)
		}
	}

	from := 0
	if q.Cursor != "" {
		_, _ = fmt.Sscanf(q.Cursor, "at:%d", &from)
	}
	size := f.pageSize
	if size <= 0 {
		size = bybit.HistoryPageSize
	}
	to := from + size
	next := fmt.Sprintf("at:%d", to)
	if to >= len(inWindow) {
		to, next = len(inWindow), ""
	}

	page := struct {
		Rows           []json.RawMessage `json:"rows"`
		NextPageCursor string            `json:"nextPageCursor"`
	}{Rows: inWindow[from:to], NextPageCursor: next}
	return json.Marshal(page)
}

func instantOf(row json.RawMessage) time.Time {
	var probe struct {
		SuccessAt  string `json:"successAt"`
		UpdateTime string `json:"updateTime"`
	}
	_ = json.Unmarshal(row, &probe)
	stamp := probe.SuccessAt
	if stamp == "" {
		stamp = probe.UpdateTime
	}
	var ms int64
	_, _ = fmt.Sscanf(stamp, "%d", &ms)
	return time.UnixMilli(ms).UTC()
}

func depositRow(id, coin, amount string, at time.Time, status int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"id":%q,"coin":%q,"amount":%q,"status":%d,"successAt":"%d","txID":"0x%s"}`,
		id, coin, amount, status, at.UnixMilli(), id))
}

func withdrawRow(id, coin, amount string, at time.Time, status string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"withdrawId":%q,"coin":%q,"amount":%q,"status":%q,"updateTime":"%d","txID":"0x%s"}`,
		id, coin, amount, status, at.UnixMilli(), id))
}

func newBybitDeps(t *testing.T, venue *fakeBybit, registry fakeRegistry) backfill.BybitDeps {
	t.Helper()
	return backfill.BybitDeps{
		DB:        appPool(t),
		Source:    venue,
		Registry:  registry,
		Now:       func() time.Time { return now },
		PageLimit: 2,
	}
}

// The walk appends every settled deposit, pages through the cursor, and stops.
func TestTheBybitDepositWalkAppendsEverySettledRow(t *testing.T) {
	target := seedIntegration(t)
	assetID := seedAsset(t)
	registry := fakeRegistry{assets: map[string]int64{"USDT": assetID}}

	venue := &fakeBybit{pageSize: 2, deposits: []json.RawMessage{
		depositRow("d1", "USDT", "100", now.Add(-10*24*time.Hour), 3),
		depositRow("d2", "USDT", "200", now.Add(-9*24*time.Hour), 3),
		depositRow("d3", "USDT", "300", now.Add(-8*24*time.Hour), 3),
	}}

	require.NoError(t, backfill.WalkBybitDeposits(
		context.Background(), newBybitDeps(t, venue, registry), target, now.Add(-20*24*time.Hour)))

	require.Equal(t,
		[]string{"bybit:deposit:d1", "bybit:deposit:d2", "bybit:deposit:d3"},
		venueIDs(events(t, target)))
	require.Greater(t, venue.calls, 1, "three rows at two a page is more than one request")
}

// The withdrawal walk exists, and this is the test that says why it may.
//
// Bybit publishes the status enum that decides whether the coins left; Binance publishes a
// garbled fragment, so its withdrawals are not walked at all (B2, F5). One venue's history is
// complete in both directions and the other's is not, by documentation rather than by effort.
func TestTheBybitWithdrawalWalkAppendsWhatBinanceCannot(t *testing.T) {
	target := seedIntegration(t)
	assetID := seedAsset(t)
	registry := fakeRegistry{assets: map[string]int64{"USDT": assetID}}

	venue := &fakeBybit{withdrawals: []json.RawMessage{
		withdrawRow("w1", "USDT", "500", now.Add(-5*24*time.Hour), "success"),
		withdrawRow("w2", "USDT", "600", now.Add(-4*24*time.Hour), "BlockchainConfirmed"),
		withdrawRow("w3", "USDT", "700", now.Add(-3*24*time.Hour), "Reject"),
	}}

	require.NoError(t, backfill.WalkBybitWithdrawals(
		context.Background(), newBybitDeps(t, venue, registry), target, now.Add(-20*24*time.Hour)))

	got := events(t, target)
	require.Equal(t, []string{"bybit:withdrawal:w1"}, venueIDs(got),
		"only `success` means the coins left; confirmed-on-chain and rejected do not")
	require.Equal(t, ledger.TypeWithdrawal, got[0].EventType)
	require.True(t, got[0].Quantity.Decimal.IsPositive(),
		"direction is on the event type, never on the sign (K49)")
}

// A movement that has not settled is skipped rather than failing the walk. It is an answer
// about something that has not happened, and the next run over the same window will get a
// different one.
func TestAnUnsettledRowDoesNotStopTheWalk(t *testing.T) {
	target := seedIntegration(t)
	assetID := seedAsset(t)
	registry := fakeRegistry{assets: map[string]int64{"USDT": assetID}}

	venue := &fakeBybit{deposits: []json.RawMessage{
		depositRow("d1", "USDT", "100", now.Add(-10*24*time.Hour), 2), // processing
		depositRow("d2", "USDT", "200", now.Add(-9*24*time.Hour), 3),  // success
	}}

	require.NoError(t, backfill.WalkBybitDeposits(
		context.Background(), newBybitDeps(t, venue, registry), target, now.Add(-20*24*time.Hour)))

	require.Equal(t, []string{"bybit:deposit:d2"}, venueIDs(events(t, target)))
}

// Every window the walk asks for is one the venue will answer: under thirty days (B3).
//
// A walk that built a wider one would be told so by the venue, and a venue error inside a
// paging loop is exactly the shape that gets mistaken for an empty page -- a gap recorded as
// a complete history, with the cursor moved past it.
func TestEveryWindowIsOneTheVenueWillAnswer(t *testing.T) {
	target := seedIntegration(t)
	registry := fakeRegistry{assets: map[string]int64{}}
	venue := &fakeBybit{}

	// Six months of history, which is more than six windows.
	require.NoError(t, backfill.WalkBybitDeposits(
		context.Background(), newBybitDeps(t, venue, registry), target, now.Add(-180*24*time.Hour)))

	require.Greater(t, len(venue.windows), 6)
	for _, w := range venue.windows {
		require.Less(t, w[1].Sub(w[0]), 30*24*time.Hour,
			"window %s..%s is wider than the venue answers for",
			w[0].Format(time.RFC3339), w[1].Format(time.RFC3339))
	}
}

// Consecutive windows OVERLAP by a second.
//
// The venue documents its query logic as second-level even though the parameters are
// milliseconds (B4), and does not say which way it rounds. Butted windows could therefore drop
// a row that sits exactly on a boundary; the overlap costs a duplicate the dedup key discards
// (L5) and removes the question.
func TestConsecutiveWindowsOverlapRatherThanButt(t *testing.T) {
	target := seedIntegration(t)
	venue := &fakeBybit{}

	require.NoError(t, backfill.WalkBybitDeposits(
		context.Background(), newBybitDeps(t, venue, fakeRegistry{}), target,
		now.Add(-90*24*time.Hour)))

	require.Greater(t, len(venue.windows), 2)
	for i := 1; i < len(venue.windows); i++ {
		previousEnd, thisStart := venue.windows[i-1][1], venue.windows[i][0]
		require.True(t, thisStart.Before(previousEnd),
			"window %d starts at %s, after the previous ended at %s -- a boundary row could fall between",
			i, thisStart.Format(time.RFC3339), previousEnd.Format(time.RFC3339))
	}
}

// A second run appends nothing new: the dedup key is the whole of L5, and the overlap above
// deliberately re-reads rows that are already in.
func TestWalkingTwiceAppendsNothingTwice(t *testing.T) {
	target := seedIntegration(t)
	assetID := seedAsset(t)
	registry := fakeRegistry{assets: map[string]int64{"USDT": assetID}}
	venue := &fakeBybit{deposits: []json.RawMessage{
		depositRow("d1", "USDT", "100", now.Add(-10*24*time.Hour), 3),
	}}
	deps := newBybitDeps(t, venue, registry)

	require.NoError(t, backfill.WalkBybitDeposits(
		context.Background(), deps, target, now.Add(-40*24*time.Hour)))
	first := venueIDs(events(t, target))

	require.NoError(t, backfill.WalkBybitDeposits(
		context.Background(), deps, target, now.Add(-40*24*time.Hour)))

	require.Equal(t, first, venueIDs(events(t, target)))
	require.Len(t, first, 1)
}

// A venue that keeps handing back the same cursor would page forever. That failure belongs to
// cursor paging and not to offset paging, so it is guarded where it can happen.
//
// The fake gives up after fifty calls rather than answering identically forever, so a walk
// without the guard FAILS here instead of hanging. A hang is the one failure a suite cannot
// report -- it has no message and no assertion, it simply never finishes -- and this walk has
// already been written once with a non-terminating loop in it.
func TestARepeatedCursorDoesNotPageForever(t *testing.T) {
	target := seedIntegration(t)
	venue := &stuckCursor{}

	err := backfill.WalkBybitDeposits(context.Background(), backfill.BybitDeps{
		DB: appPool(t), Source: venue, Registry: fakeRegistry{},
		Now: func() time.Time { return now },
	}, target, now.Add(-10*24*time.Hour))

	require.NoError(t, err, "the walk kept asking until the venue gave up")
	require.LessOrEqual(t, venue.calls, 4,
		"the walk noticed the cursor was not advancing and stopped")
}

// stuckCursor answers every page with the same cursor -- the venue misbehaving in the one way
// a cursor-paged loop cannot survive on its own -- and refuses after fifty calls so a broken
// walk is bounded and observable rather than infinite and silent.
type stuckCursor struct{ calls int }

const stuckCursorPatience = 50

func (s *stuckCursor) DepositRecords(context.Context, bybit.HistoryQuery) (json.RawMessage, error) {
	s.calls++
	if s.calls > stuckCursorPatience {
		return nil, errors.New("fake venue: asked the same page fifty times")
	}
	return json.RawMessage(`{"rows":[],"nextPageCursor":"always-the-same"}`), nil
}

func (s *stuckCursor) WithdrawRecords(
	ctx context.Context, q bybit.HistoryQuery,
) (json.RawMessage, error) {
	return s.DepositRecords(ctx, q)
}

// The walk terminates, and this test is here because the first version did not.
//
// The overlap that keeps a boundary row from being dropped stepped back one second from the
// final window's end, found itself before `end` again, and walked the same window forever.
// A non-terminating loop leaves no error message and no failing assertion -- it just stops,
// which is the one failure mode a test suite reports as "still running". So the bound is
// asserted directly rather than trusted.
func TestTheWalkTerminatesAndDoesNotRewalkItsFinalWindow(t *testing.T) {
	target := seedIntegration(t)
	venue := &fakeBybit{}

	done := make(chan error, 1)
	go func() {
		done <- backfill.WalkBybitDeposits(
			context.Background(), newBybitDeps(t, venue, fakeRegistry{}), target,
			now.Add(-90*24*time.Hour))
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the walk did not finish: it is re-walking a window")
	}

	// Ninety days in windows of under thirty is four, and no number of overlaps makes it
	// more than a handful. A walk that looped would have asked for hundreds.
	require.LessOrEqual(t, len(venue.windows), 6,
		"asked for %d windows to cover ninety days", len(venue.windows))
}
