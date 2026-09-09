package marketdata_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/marketdata"
	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// spyLimiter records what it was charged. The point of the assertion is not that a limiter
// exists but that the documented weight was spent: a call charged 1 where the venue counts
// 4 exhausts the shared IP budget three times faster than the code believes (K24, F9).
type spyLimiter struct {
	charged []int
	ids     []uuid.UUID
}

func (s *spyLimiter) Acquire(_ context.Context, id uuid.UUID, _ ratelimit.Priority, weight int) error {
	s.charged = append(s.charged, weight)
	s.ids = append(s.ids, id)
	return nil
}

func newClient(t *testing.T, handler http.HandlerFunc) (*marketdata.Client, *spyLimiter) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	limiter := &spyLimiter{}
	c, err := marketdata.NewClient(srv.URL, limiter, srv.Client())
	require.NoError(t, err)
	return c, limiter
}

func TestPricesReadsEverySymbolAndChargesTheDocumentedWeight(t *testing.T) {
	c, limiter := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/ticker/price", r.URL.Path)
		require.Empty(t, r.URL.RawQuery, "omitting symbol is what returns all of them")
		require.Empty(t, r.Header.Get("X-MBX-APIKEY"), "market data carries no key (F6)")
		_, _ = w.Write([]byte(`[{"symbol":"BTCUSDT","price":"60123.45000000"},
		                        {"symbol":"ETHUSDT","price":"2500.10000000"}]`))
	})

	got, err := c.Prices(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "BTCUSDT", got[0].Symbol)
	require.Equal(t, "60123.45", got[0].Price.String())
	require.False(t, got[0].ObservedAt.IsZero(), "a snapshot is a statement about now")

	require.Equal(t, []int{4}, limiter.charged, "ticker/price with no symbol is weight 4 (F9)")
	require.Equal(t, []uuid.UUID{marketdata.FeedID}, limiter.ids,
		"the feed is charged to its own identity, not to an account")
}

// A kline is identified by its OPEN time, and its close price is the price at the end of
// that minute. Filing it under the open time is what makes a backfilled minute agree with a
// recorded one; filing it under the close would shift every backfilled price one minute
// into the future, which is look-ahead bias baked into history.
func TestKlinesFileTheClosePriceUnderTheOpenMinute(t *testing.T) {
	openMillis := time.Date(2026, 9, 9, 14, 37, 0, 0, time.UTC).UnixMilli()

	c, limiter := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/klines", r.URL.Path)
		require.Equal(t, "1m", r.URL.Query().Get("interval"))
		require.Equal(t, "1000", r.URL.Query().Get("limit"), "the documented maximum (F9)")
		_, _ = w.Write([]byte(`[[` + strconv.FormatInt(openMillis, 10) + `,"59000.00","61000.00","58000.00",
		                         "60123.45","100",1789041479999,"6000000",120,"50","3000000","0"]]`))
	})

	got, err := c.Klines(context.Background(), "BTCUSDT", time.Now().Add(-time.Hour), time.Now(), 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "60123.45", got[0].Price.String(), "index 4 is the close price")
	require.Equal(t, openMillis, got[0].ObservedAt.UnixMilli(), "index 0 is the open time")
	require.Equal(t, []int{2}, limiter.charged)
}

// A truncated kline row is refused rather than read past the end. Guessing a missing close
// price would put a zero into a price table.
func TestATruncatedKlineIsRefused(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[[1789041420000,"59000.00"]]`))
	})

	_, err := c.Klines(context.Background(), "BTCUSDT", time.Now().Add(-time.Hour), time.Now(), 10)
	require.Error(t, err)
	require.Contains(t, err.Error(), "BTCUSDT")
}

// A non-200 is an error carrying the body. The venue explains its refusals in the body, and
// discarding it turns a fixable problem into "the feed stopped working".
func TestAnErrorResponseCarriesWhatTheVenueSaid(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":-1003,"msg":"Too many requests."}`))
	})

	_, err := c.Prices(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "-1003")
	require.Contains(t, err.Error(), "429")
}

// L1 at the edge: a price the venue sent as a JSON number rather than a string means
// something reparsed it on the way, and the digits can no longer be trusted.
func TestAPriceThatIsNotAStringIsRefusedBySnapshotToo(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"symbol":"BTCUSDT","price":60123.45}]`))
	})

	_, err := c.Prices(context.Background())
	require.Error(t, err)
}
