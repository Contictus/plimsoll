package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// FuturesStreamURL is the USD-M user data endpoint, quoted from the migration notice (F17).
//
// This constant is the whole reason F17 exists. The legacy URLs were decommissioned on
// 2026-04-23, and a stream written from memory connects to one of them successfully and
// receives nothing -- which looks exactly like an account with no activity, and would be
// discovered by someone wondering why their futures fills were hours late.
const FuturesStreamURL = "wss://fstream.binance.com/private"

// listenKeyPath mints and refreshes the key. Weight 1 on all three verbs, signed with the
// API key header only.
const listenKeyPath = "/fapi/v1/listenKey"

// listenKeyRefresh is how often the key is renewed. The venue says: "The stream will close
// after 60 minutes unless a keepalive is sent", and recommends a ping about every 60
// minutes. Refreshing AT the expiry is a race with the venue's own clock, so this is half of
// it: two refreshes may fail before the stream is lost.
const listenKeyRefresh = 30 * time.Minute

// weightListenKey is the documented cost of each of the three verbs.
const weightListenKey = 1

// FuturesStreamConfig is one account's USD-M user feed, assembled.
type FuturesStreamConfig struct {
	IntegrationID uuid.UUID

	// Client mints and refreshes the listenKey. The same client the walks use, so the
	// keepalive is charged to the same weight budget as everything else (K24).
	Client *Client

	// URL defaults to FuturesStreamURL. Injected so tests point at httptest.
	URL string

	Now     func() time.Time
	Backoff func(attempt int) time.Duration
}

// FuturesStream is the live USD-M user feed. It satisfies the same shape the supervisor
// already drives for spot, so nothing above it has to know which venue path it came from.
type FuturesStream struct {
	cfg FuturesStreamConfig

	messages  chan Message
	closed    chan struct{}
	closeOnce sync.Once
	done      chan struct{}

	mu     sync.Mutex
	conn   *websocket.Conn
	cancel context.CancelFunc

	started   atomic.Bool
	connected atomic.Bool
}

// NewFuturesStream validates the configuration. It opens nothing.
func NewFuturesStream(cfg FuturesStreamConfig) (*FuturesStream, error) {
	if cfg.IntegrationID == uuid.Nil {
		return nil, errors.New("binance: futures stream needs an integration id")
	}
	if cfg.Client == nil {
		return nil, errors.New("binance: futures stream needs a client to mint its listenKey")
	}
	if cfg.URL == "" {
		cfg.URL = FuturesStreamURL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Backoff == nil {
		cfg.Backoff = defaultBackoff
	}
	return &FuturesStream{
		cfg:      cfg,
		messages: make(chan Message, 64),
		closed:   make(chan struct{}),
		done:     make(chan struct{}),
	}, nil
}

// StartUserStream mints a listenKey. Weight 1, and signed with the API key header only --
// there is no signature on this endpoint.
func (c *Client) StartUserStream(ctx context.Context) (string, error) {
	body, err := c.do(ctx, request{
		path:    listenKeyPath,
		method:  "POST",
		weight:  weightListenKey,
		keyed:   true,
		futures: true,
	})
	if err != nil {
		return "", fmt.Errorf("binance: start futures user stream: %w", err)
	}
	var payload struct {
		ListenKey string `json:"listenKey"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("binance: decode listenKey: %w", err)
	}
	if payload.ListenKey == "" {
		return "", errors.New("binance: the venue returned no listenKey")
	}
	return payload.ListenKey, nil
}

// KeepUserStreamAlive refreshes the key. Weight 1.
//
// The key itself is never logged and never wrapped into an error: it is a bearer token for
// this account's order flow, and the error a failed keepalive produces is the string most
// likely to reach a log (L13).
func (c *Client) KeepUserStreamAlive(ctx context.Context) error {
	if _, err := c.do(ctx, request{
		path:    listenKeyPath,
		method:  "PUT",
		weight:  weightListenKey,
		keyed:   true,
		futures: true,
	}); err != nil {
		return fmt.Errorf("binance: keep futures user stream alive: %w", err)
	}
	return nil
}

// streamURL puts the key in the query string, as the migration notice documents, and filters
// server-side to the one event this build acts on.
func (s *FuturesStream) streamURL(listenKey string) string {
	query := url.Values{}
	query.Set("listenKey", listenKey)
	query.Set("events", "ORDER_TRADE_UPDATE")
	return s.cfg.URL + "?" + query.Encode()
}

// Subscribe mints a listenKey, connects, and delivers events until the stream is closed.
//
// The first connect happens before it returns, so a rejected credential reaches the caller
// instead of disappearing into a retry loop.
func (s *FuturesStream) Subscribe(ctx context.Context) (<-chan Message, error) {
	runCtx, cancel := context.WithCancel(ctx)
	conn, err := s.connect(runCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	s.setConn(conn)
	s.connected.Store(true)

	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	s.started.Store(true)
	go s.run(runCtx)
	go s.keepAlive(runCtx)
	return s.messages, nil
}

// Connected reports whether the stream is currently delivering.
func (s *FuturesStream) Connected() bool { return s.connected.Load() }

// Close ends the stream. Safe to call more than once.
func (s *FuturesStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)

		s.mu.Lock()
		conn, cancel := s.conn, s.cancel
		s.conn = nil
		s.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if conn != nil {
			_ = conn.CloseNow()
		}
		if s.started.Load() {
			<-s.done
		}
	})
	return nil
}

// connect mints a fresh listenKey and dials. A new key per connect rather than a stored one:
// a key that expired while the connection was down would dial successfully and deliver
// nothing, which is the failure mode this whole file exists to avoid (F17).
func (s *FuturesStream) connect(ctx context.Context) (*websocket.Conn, error) {
	listenKey, err := s.cfg.Client.StartUserStream(ctx)
	if err != nil {
		return nil, err
	}
	// The handshake response is not ours to close, quoted from the library's own
	// documentation: "You never need to close resp.Body yourself." Closing it would close
	// the hijacked connection.
	conn, _, err := websocket.Dial(ctx, s.streamURL(listenKey), nil) //nolint:bodyclose // see above
	if err != nil {
		// The URL carries the listenKey, so it is described rather than wrapped (L13).
		return nil, errors.New("binance: could not connect to the futures user stream")
	}
	// The same read limit spot uses: the default 32 KiB is smaller than an account update on
	// a book holding many positions.
	conn.SetReadLimit(4 << 20)
	return conn, nil
}

// keepAlive refreshes the listenKey on a ticker. A failure is not fatal: the next reconnect
// mints a new key, and stopping the stream over one refused keepalive would turn a recovered
// blip into an outage.
func (s *FuturesStream) keepAlive(ctx context.Context) {
	ticker := time.NewTicker(listenKeyRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-ticker.C:
			_ = s.cfg.Client.KeepUserStreamAlive(ctx)
		}
	}
}

// run reads frames until the connection fails, then reconnects and reports the gap.
func (s *FuturesStream) run(ctx context.Context) {
	defer close(s.messages)
	defer close(s.done)
	defer s.connected.Store(false)

	for attempt := 1; ; {
		s.readUntilFailure(ctx)
		s.connected.Store(false)
		gapFrom := s.cfg.Now()

		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-time.After(s.cfg.Backoff(attempt)):
		}

		conn, err := s.connect(ctx)
		if err != nil {
			attempt++
			continue
		}
		attempt = 1
		s.setConn(conn)
		s.connected.Store(true)

		// The window the account was unobserved for. The supervisor replays it over REST:
		// the stream is a latency improvement, and the REST walk is what makes it whole.
		if !s.emit(Message{Err: &GapError{From: gapFrom, To: s.cfg.Now()}}) {
			return
		}
	}
}

func (s *FuturesStream) readUntilFailure(ctx context.Context) {
	for {
		conn := s.currentConn()
		if conn == nil {
			return
		}
		_, frame, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if !s.emit(Message{Event: json.RawMessage(frame)}) {
			return
		}
	}
}

func (s *FuturesStream) emit(msg Message) bool {
	select {
	case s.messages <- msg:
		return true
	case <-s.closed:
		return false
	}
}

func (s *FuturesStream) setConn(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		_ = s.conn.CloseNow()
	}
	s.conn = conn
}

func (s *FuturesStream) currentConn() *websocket.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

// FuturesEventSymbol reads the symbol out of a user data frame.
//
// Only the two fields this build acts on are read: the event type and the symbol inside the
// order object. Everything else about the payload -- the trade id, the fill price, the
// commission -- is deliberately NOT parsed here, because the identity of a fill (L5) must
// match what the REST walk mints, and this build has not verified those field names against
// the venue's own page. So the stream says WHICH symbol moved and WHEN, and the REST walk
// says what happened: a latency improvement that cannot invent a number.
func FuturesEventSymbol(frame json.RawMessage) (string, bool) {
	// Decoded key by key rather than into a tagged struct, and this is not style.
	//
	// encoding/json matches field names CASE-INSENSITIVELY. On this venue's payloads that is
	// a correctness bug waiting to happen: the event carries both "e" (event type) and "E"
	// (event time), and the order object carries both "s" (symbol) and "S" (side). A struct
	// tagged `json:"s"` is filled by whichever the decoder reaches -- in practice the side,
	// so the symbol comes out as "BUY" and the trigger resyncs a contract that does not
	// exist. A map lookup is exact.
	var eventType string
	if raw, ok := jsonField(frame, "e"); !ok {
		return "", false
	} else if err := json.Unmarshal(raw, &eventType); err != nil {
		return "", false
	}
	if eventType != "ORDER_TRADE_UPDATE" {
		return "", false
	}

	order, ok := jsonField(frame, "o")
	if !ok {
		return "", false
	}
	raw, ok := jsonField(order, "s")
	if !ok {
		return "", false
	}
	var symbol string
	if err := json.Unmarshal(raw, &symbol); err != nil || symbol == "" {
		return "", false
	}
	return symbol, true
}

// jsonField reads one key exactly as spelled.
func jsonField(payload json.RawMessage, key string) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, false
	}
	value, ok := fields[key]
	return value, ok
}
