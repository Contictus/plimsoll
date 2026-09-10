package collateral_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/collateral"
	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/stretchr/testify/require"
)

const bracketsPayload = `[{"symbol":"BTCUSDT","brackets":[
  {"bracket":1,"notionalFloor":0,"notionalCap":50000,"maintMarginRatio":0.004,"cum":0.0},
  {"bracket":2,"notionalFloor":50000,"notionalCap":250000,"maintMarginRatio":0.005,"cum":50.0}]}]`

// fakeVenue answers the three calls a capture makes, and can be told to take time over one
// of them -- which is how a test moves the clock between the two halves without sleeping.
type fakeVenue struct {
	account, positions, brackets string
	failOn                       string
	clock                        *time.Time
	delayBeforePositions         time.Duration
	calls                        []string
}

var errVenue = errors.New("fake venue: injected failure")

func (v *fakeVenue) answer(name, body string) (json.RawMessage, error) {
	v.calls = append(v.calls, name)
	if v.failOn == name {
		return nil, errVenue
	}
	return json.RawMessage(body), nil
}

func (v *fakeVenue) FuturesAccount(context.Context) (json.RawMessage, error) {
	return v.answer("account", v.account)
}

func (v *fakeVenue) PositionRisk(context.Context) (json.RawMessage, error) {
	if v.clock != nil {
		*v.clock = v.clock.Add(v.delayBeforePositions)
	}
	return v.answer("positions", v.positions)
}

func (v *fakeVenue) LeverageBracket(context.Context) (json.RawMessage, error) {
	return v.answer("brackets", v.brackets)
}

type fakeInstruments struct {
	markets []instrument.Market
}

func (f *fakeInstruments) Instrument(
	_ context.Context, market instrument.Market, _ string, _ time.Time,
) (int64, error) {
	f.markets = append(f.markets, market)
	return 777, nil
}

func newVenue() *fakeVenue {
	return &fakeVenue{account: accountPayload, positions: positionsPayload, brackets: bracketsPayload}
}

func captureWith(t *testing.T, v *fakeVenue, clock *time.Time, tolerance time.Duration) (collateral.Snapshot, error) {
	t.Helper()
	v.clock = clock
	return collateral.Capture(context.Background(), v, &fakeInstruments{},
		func() time.Time { return *clock }, tolerance)
}

// One capture, one instant, both halves. The maintenance requirement comes from the account
// call and the liquidation price from the position call (F14), so this is the test that they
// arrive as one answer rather than as two.
func TestACaptureAsksBothHalvesAndPairsThem(t *testing.T) {
	clock := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	v := newVenue()
	s, err := captureWith(t, v, &clock, time.Minute)
	require.NoError(t, err)

	// The two halves of the pair are asked back to back, with the tier table -- which is a
	// schedule rather than a fact about this instant -- fetched afterwards so a slow
	// response to it cannot tear the snapshot.
	require.Equal(t, []string{"account", "positions", "brackets"}, v.calls)
	require.Equal(t, "10250", s.MarginBalance.String())
	require.Equal(t, "1500", s.MaintenanceMargin.String())
	require.Len(t, s.Positions, 1)
	require.Equal(t, int64(777), s.Positions[0].InstrumentID)

	// The maintenance margin ON THE POSITION comes from the bracket table rather than from
	// the venue's per-position field, so the number M7.5 shocks is produced the same way
	// today's is. 30500 sits in the second tier: 30500 * 0.004 - 0.
	require.Equal(t, "122", s.Positions[0].MaintMargin.String())
}

// The tolerance exists because the two calls are two round trips. A gap wider than it means
// the pair no longer describes one moment, and a margin buffer beside a liquidation price
// from a different minute is one screen describing two different accounts.
func TestACaptureThatToreIsRefused(t *testing.T) {
	clock := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	v := newVenue()
	v.delayBeforePositions = 5 * time.Minute

	_, err := captureWith(t, v, &clock, time.Minute)
	require.ErrorIs(t, err, collateral.ErrSnapshotTorn)
}

// A failure on either half fails the capture. Half a snapshot is not a degraded snapshot: a
// margin balance with no positions reads as an account holding nothing, which is the safest
// possible number and completely wrong.
func TestAFailureOnEitherHalfFailsTheCapture(t *testing.T) {
	for _, call := range []string{"account", "positions", "brackets"} {
		clock := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
		v := newVenue()
		v.failOn = call

		_, err := captureWith(t, v, &clock, time.Minute)
		require.ErrorIs(t, err, errVenue, "a failure on %s did not fail the capture", call)
	}
}

// The instrument is resolved in the USD-M market, as of the snapshot's own instant. Resolving
// in the spot market would attach a leveraged position's risk to a spot pair; resolving at
// write time would use whatever mapping is current then (L8).
func TestACaptureResolvesInTheFuturesMarket(t *testing.T) {
	clock := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	r := &fakeInstruments{}
	_, err := collateral.Capture(context.Background(), newVenue(), r,
		func() time.Time { return clock }, time.Minute)
	require.NoError(t, err)

	require.NotEmpty(t, r.markets)
	require.Equal(t, instrument.MarketUSDM, r.markets[0])
}
