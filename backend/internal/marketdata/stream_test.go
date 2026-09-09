package marketdata_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/marketdata"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// wsServer runs a websocket endpoint that pushes whatever the test tells it to.
func wsServer(t *testing.T, handle func(ctx context.Context, c *websocket.Conn)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		handle(r.Context(), c)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func push(ctx context.Context, c *websocket.Conn, payload string) error {
	return c.Write(ctx, websocket.MessageText, []byte(payload))
}

func TestTheStreamDeliversDecodedQuotes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := wsServer(t, func(ctx context.Context, c *websocket.Conn) {
		_ = push(ctx, c, miniTickerArray)
		<-ctx.Done()
	})

	s := marketdata.NewStream(marketdata.StreamConfig{URL: url})
	go func() { _ = s.Run(ctx) }()
	t.Cleanup(func() { _ = s.Close() })

	select {
	case frame := <-s.Frames():
		require.Len(t, frame.Quotes, 2)
		require.Equal(t, "BTCUSDT", frame.Quotes[0].Symbol)
		require.Equal(t, "60123.45", frame.Quotes[0].Price.String())
	case <-ctx.Done():
		t.Fatal("no frame arrived")
	}
	require.True(t, s.Connected())
}

// A dropped connection is routine -- the venue disconnects at 24 hours by documentation --
// so a stream that does not reconnect stops working every day by design.
func TestTheStreamReconnectsAfterTheConnectionDrops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	connections := make(chan struct{}, 4)
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn) {
		connections <- struct{}{}
		_ = push(ctx, c, miniTickerArray)
		// Drop immediately: the client must come back on its own.
	})

	s := marketdata.NewStream(marketdata.StreamConfig{
		URL:     url,
		Backoff: func(int) time.Duration { return time.Millisecond },
	})
	go func() { _ = s.Run(ctx) }()
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < 2; i++ {
		select {
		case <-connections:
		case <-ctx.Done():
			t.Fatalf("only %d connection(s) were made; the stream did not reconnect", i)
		}
	}
}

// F8: the venue announces its own shutdown, which the user-data stream has no equivalent
// of. Acting on it reconnects on our schedule instead of waiting for the socket to drop.
func TestAServerShutdownTriggersAReconnectRatherThanAStall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	connections := make(chan struct{}, 4)
	url := wsServer(t, func(ctx context.Context, c *websocket.Conn) {
		connections <- struct{}{}
		_ = push(ctx, c, `{"e":"serverShutdown","E":1789041420000}`)
		// The server does NOT close. A client that ignored the event would sit here.
		<-ctx.Done()
	})

	s := marketdata.NewStream(marketdata.StreamConfig{
		URL:     url,
		Backoff: func(int) time.Duration { return time.Millisecond },
	})
	go func() { _ = s.Run(ctx) }()
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < 2; i++ {
		select {
		case <-connections:
		case <-ctx.Done():
			t.Fatal("the stream stayed on a connection the venue said was ending")
		}
	}
}

// One unreadable frame is a payload we do not understand. Tearing the feed down over it
// would turn a gap in one symbol into a gap in all of them -- so it is counted and skipped,
// and the counter is what tells an operator the venue changed something.
func TestAnUnreadableFrameIsCountedAndTheFeedContinues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := wsServer(t, func(ctx context.Context, c *websocket.Conn) {
		_ = push(ctx, c, `{"e":"somethingNobodyHasSeen","E":1}`)
		_ = push(ctx, c, miniTickerArray)
		<-ctx.Done()
	})

	s := marketdata.NewStream(marketdata.StreamConfig{URL: url})
	go func() { _ = s.Run(ctx) }()
	t.Cleanup(func() { _ = s.Close() })

	select {
	case frame := <-s.Frames():
		require.Len(t, frame.Quotes, 2, "the good frame after the bad one must still arrive")
	case <-ctx.Done():
		t.Fatal("the feed stopped at the unreadable frame")
	}
	require.Equal(t, int64(1), s.BadFrames())
}

// Close must not return while the run goroutine is still alive, or "stopped" would be a
// claim about intent rather than about state.
func TestCloseWaitsForTheStreamToStop(t *testing.T) {
	ctx := context.Background()
	url := wsServer(t, func(ctx context.Context, _ *websocket.Conn) { <-ctx.Done() })

	s := marketdata.NewStream(marketdata.StreamConfig{URL: url})
	go func() { _ = s.Run(ctx) }()

	require.Eventually(t, s.Connected, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, s.Close())

	_, open := <-s.Frames()
	require.False(t, open, "the frame channel must be closed once the stream has stopped")
}
