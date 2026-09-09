//go:build live

// Behind the `live` tag, which no gate runs. It is the market-data equivalent of
// `plimsollctl record`: a way to check on demand that the venue still behaves the way
// docs/BINANCE-API-NOTES.md §6 says it does.
//
// Run it deliberately:
//
//	go test -tags=live -count=1 -v ./internal/marketdata/
//
// It touches only public endpoints -- no key, no signature, no account (F6) -- so it can be
// run by anyone, on any machine, without a credential existing anywhere.
package marketdata_test

import (
	"context"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/marketdata"
	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type openLimiter struct{}

func (openLimiter) Acquire(context.Context, uuid.UUID, ratelimit.Priority, int) error { return nil }

// The snapshot the recorder cannot start without (F7).
func TestLiveSnapshotReturnsEverySymbol(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := marketdata.NewClient("", openLimiter{}, nil)
	require.NoError(t, err)

	quotes, err := c.Prices(ctx)
	require.NoError(t, err)
	require.Greater(t, len(quotes), 100, "ticker/price with no symbol returns the whole market")

	bySymbol := map[string]marketdata.Quote{}
	for _, q := range quotes {
		bySymbol[q.Symbol] = q
	}
	btc, ok := bySymbol["BTCUSDT"]
	require.True(t, ok, "BTCUSDT must be in an all-market snapshot")
	require.True(t, btc.Price.IsPositive(), "got %s", btc.Price)
	t.Logf("snapshot: %d symbols, BTCUSDT=%s", len(quotes), btc.Price)
}

// The stream name and payload shape, against the real venue. This is the assertion a
// documentation change would break, and the reason this file exists.
func TestLiveStreamDeliversMiniTickers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	s := marketdata.NewStream(marketdata.StreamConfig{})
	go func() { _ = s.Run(ctx) }()
	defer func() { _ = s.Close() }()

	select {
	case frame := <-s.Frames():
		require.NotEmpty(t, frame.Quotes, "a push must decode to at least one quote")
		q := frame.Quotes[0]
		require.NotEmpty(t, q.Symbol)
		require.True(t, q.Price.IsPositive(), "got %s for %s", q.Price, q.Symbol)
		require.WithinDuration(t, time.Now(), q.ObservedAt, time.Minute,
			"the venue's event time must be current; if it is not, E is not what we think")
		t.Logf("stream: %d quotes, first %s=%s at %s",
			len(frame.Quotes), q.Symbol, q.Price, q.ObservedAt.Format(time.RFC3339))
	case <-ctx.Done():
		t.Fatal("no frame arrived from the live stream in 45s")
	}
	require.Zero(t, s.BadFrames(), "every frame the venue sent must decode")
}

// Klines at 1m, which is what fills a gap and answers a past instant (F9, K7).
func TestLiveKlinesAreMinutesOldestFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := marketdata.NewClient("", openLimiter{}, nil)
	require.NoError(t, err)

	to := time.Now().UTC()
	from := to.Add(-30 * time.Minute)
	quotes, err := c.Klines(ctx, "BTCUSDT", from, to, 0)
	require.NoError(t, err)
	require.NotEmpty(t, quotes)

	for i := 1; i < len(quotes); i++ {
		require.True(t, quotes[i].ObservedAt.After(quotes[i-1].ObservedAt), "oldest first")
		require.Equal(t, time.Minute, quotes[i].ObservedAt.Sub(quotes[i-1].ObservedAt),
			"interval=1m must produce one-minute steps")
	}
	t.Logf("klines: %d minutes, last %s at %s",
		len(quotes), quotes[len(quotes)-1].Price, quotes[len(quotes)-1].ObservedAt.Format(time.RFC3339))
}
