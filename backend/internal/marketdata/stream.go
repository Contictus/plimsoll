package marketdata

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	// DefaultStreamURL is the market-data-only endpoint. F6 quotes it: this host "can be
	// subscribed to receive only market data messages. User data stream is NOT available
	// from this URL."
	//
	// Chosen over stream.binance.com deliberately. It makes "this connection cannot carry
	// account data" a property of the transport rather than a promise the code keeps --
	// the same reasoning that puts the ledger's append-only rule in a GRANT.
	DefaultStreamURL = "wss://data-stream.binance.vision/ws/!miniTicker@arr"

	streamDialTimeout = 30 * time.Second

	// readLimit guards against a frame large enough to exhaust memory. !miniTicker@arr
	// carries every symbol that moved in the last second, which on a busy market is
	// thousands of entries, so the 32 KiB default is far too small.
	readLimit = 8 << 20

	// The venue disconnects at the 24-hour mark by documentation (F8). Reconnecting a
	// little early costs one reconnect; being late costs a gap, and the two are not
	// comparable.
	connectionLifetime = 24 * time.Hour
	reconnectBefore    = 5 * time.Minute

	backoffCap = 30 * time.Second
)

// StreamConfig is what a Stream needs. Every field has a default, because the feed needs no
// credential and no account -- which is the whole point of it.
type StreamConfig struct {
	// URL defaults to DefaultStreamURL. Injected so tests point at httptest.
	URL string
	// Backoff is the wait before reconnect attempt n (1-based).
	Backoff func(attempt int) time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

// Stream is the public price feed.
//
// It is not built on binance.Stream, and the difference is not incidental: that one signs a
// subscribe request, holds a credential, and is charged to an integration. This one has no
// handshake at all -- the stream name is in the URL -- carries no key, and belongs to the
// process rather than to an account. Sharing an abstraction would mean a type whose every
// field is optional depending on which of two protocols it is speaking.
type Stream struct {
	url     string
	backoff func(attempt int) time.Duration
	now     func() time.Time

	frames    chan Frame
	closed    chan struct{}
	closeOnce sync.Once
	done      chan struct{}
	cancel    context.CancelFunc

	connected  atomic.Bool
	reconnects atomic.Int64
	badFrames  atomic.Int64
	started    atomic.Bool
}

// NewStream builds a stream. It connects nothing; Run does that.
func NewStream(cfg StreamConfig) *Stream {
	if cfg.URL == "" {
		cfg.URL = DefaultStreamURL
	}
	if cfg.Backoff == nil {
		cfg.Backoff = defaultStreamBackoff
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Stream{
		url:     cfg.URL,
		backoff: cfg.Backoff,
		now:     cfg.Now,
		frames:  make(chan Frame, 16),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func defaultStreamBackoff(attempt int) time.Duration {
	d := 500 * time.Millisecond * (1 << (attempt - 1))
	if d > backoffCap || d <= 0 {
		return backoffCap
	}
	return d
}

// Frames is the channel decoded pushes arrive on. It is closed when the stream stops.
func (s *Stream) Frames() <-chan Frame { return s.frames }

// Connected reports whether a connection is currently established. It is what a freshness
// reason is built from: a feed that is down is a price that is ageing.
func (s *Stream) Connected() bool { return s.connected.Load() }

// Reconnects is how many times the connection has been re-established.
func (s *Stream) Reconnects() int64 { return s.reconnects.Load() }

// BadFrames is how many pushes could not be decoded. Read together with Reconnects: a
// rising BadFrames with a stable Reconnects means the venue changed a payload, which is a
// different problem from a flaky network and must not look like one.
func (s *Stream) BadFrames() int64 { return s.badFrames.Load() }

// Run connects and keeps the feed alive until ctx is cancelled or Close is called.
//
// A dropped connection is routine rather than exceptional: the venue disconnects at the
// 24-hour mark by documentation (F8), so a stream that does not reconnect stops working
// every day by design.
func (s *Stream) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("marketdata: stream has already been run")
	}

	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()
	defer close(s.done)
	defer close(s.frames)

	attempt := 0
	for {
		if runCtx.Err() != nil {
			return nil
		}

		conn, err := s.connect(runCtx)
		if err != nil {
			attempt++
			if !s.wait(runCtx, s.backoff(attempt)) {
				return nil
			}
			continue
		}
		attempt = 0
		s.connected.Store(true)

		s.readUntilFailure(runCtx, conn)

		s.connected.Store(false)
		_ = conn.CloseNow()
		s.reconnects.Add(1)
	}
}

// Close stops the stream and waits for its goroutine to finish, so a caller that returns
// after Close knows nothing further will be written.
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.cancel != nil {
			s.cancel()
		}
	})
	if !s.started.Load() {
		return nil
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

func (s *Stream) connect(ctx context.Context) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, streamDialTimeout)
	defer cancel()

	// bodyclose is wrong here, and the suppression is narrow on purpose. Dial's own
	// documentation: "The response is the WebSocket handshake response from the server. You
	// never need to close resp.Body yourself."
	conn, _, err := websocket.Dial(dialCtx, s.url, nil) //nolint:bodyclose // see above
	if err != nil {
		return nil, fmt.Errorf("marketdata: dial %s: %w", s.url, err)
	}
	conn.SetReadLimit(readLimit)
	return conn, nil
}

// readUntilFailure reads frames until the connection fails, the venue announces a shutdown,
// or the reconnect deadline arrives. It returns so the caller can reconnect.
func (s *Stream) readUntilFailure(ctx context.Context, conn *websocket.Conn) {
	deadline := s.now().Add(connectionLifetime - reconnectBefore)

	for {
		if s.now().After(deadline) {
			return
		}
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}

		frame, err := DecodeFrame(raw)
		if err != nil {
			// Counted, not fatal. One unreadable frame is a payload we do not understand;
			// tearing the feed down over it would turn a gap in one symbol into a gap in
			// all of them. A rising counter is what says the venue changed something.
			s.badFrames.Add(1)
			continue
		}
		if frame.Shutdown {
			// F8: the venue says it is going away. Returning now reconnects on our
			// schedule rather than waiting for the socket to drop on its own.
			return
		}
		if len(frame.Quotes) == 0 {
			continue
		}
		if !s.emit(ctx, frame) {
			return
		}
	}
}

// emit hands a frame to the consumer.
//
// It blocks on the consumer, and that is deliberate: dropping a frame would silently lose
// prices, and the consumer here writes to Postgres rather than doing anything unbounded.
// The escape hatches are the ones that mean the feed is over anyway.
func (s *Stream) emit(ctx context.Context, frame Frame) bool {
	select {
	case s.frames <- frame:
		return true
	case <-ctx.Done():
		return false
	case <-s.closed:
		return false
	}
}

func (s *Stream) wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	case <-s.closed:
		return false
	}
}
