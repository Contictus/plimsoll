package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/integration"
	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Spot user data arrives on the WebSocket API, not on a listenKey stream. listenKey
// documentation for wss://stream.binance.com was removed after the 2025-04-07 announcement
// (F1). There is deliberately no listenKey lifecycle in this file: if one appears here, it
// was copied from a stale tutorial. USD-M still uses listenKey and is a separate path.
//
// Everything below was read from the official documentation on 2026-09-04:
// web-socket-api.md (subscription, connection lifetime, weights) and user-data-stream.md
// (the event envelope).
const (
	// SpotStreamURL is the WebSocket API endpoint, quoted from the docs.
	SpotStreamURL = "wss://ws-api.binance.com:443/ws-api/v3"

	// subscribeMethod is the signature variant, which works with the HMAC key we already
	// store. The other variant, userDataStream.subscribe, requires an authenticated session
	// established with session.logon, and that requires Ed25519 keys -- a change to the
	// credential model (K25) that buys nothing here.
	subscribeMethod = "userDataStream.subscribe.signature"

	// weightSubscribe is the documented IP weight of one subscribe request.
	weightSubscribe = 2

	// streamDialTimeout bounds a single dial. The reconnect loop retries; a dial with no
	// timeout would simply stop retrying.
	streamDialTimeout = 30 * time.Second

	// subscribeTimeout bounds the wait for the subscribe response.
	subscribeTimeout = 30 * time.Second
)

// ErrGap means events were missed. The stream reports it on reconnect: nothing in the
// protocol says what happened while the connection was down, and treating that window as
// empty is the silent data loss L11 exists to prevent.
var ErrGap = errors.New("binance: user data stream gap")

// ErrSubscribeRejected means the venue refused the subscription -- a bad key, a bad
// signature, a timestamp outside recvWindow. Not a transport failure: retrying it forever
// would hide a credential problem behind a reconnect loop and spend weight doing it.
var ErrSubscribeRejected = errors.New("binance: user data subscription rejected")

// GapError carries the window that was missed, which is what a bounded resync needs. From
// is the last moment the stream is known to have been delivering; To is when it was
// delivering again.
type GapError struct {
	From, To time.Time
}

func (e *GapError) Error() string {
	return fmt.Sprintf("binance: user data stream gap from %s to %s",
		e.From.UTC().Format(time.RFC3339Nano), e.To.UTC().Format(time.RFC3339Nano))
}

func (e *GapError) Unwrap() error { return ErrGap }

// Message is one thing the stream has to tell its consumer: an event, or a gap where events
// should have been.
//
// The plan called for a channel of json.RawMessage. A raw message cannot carry a gap, and a
// gap that the consumer never hears about is exactly the failure this stream exists to
// avoid -- so the channel carries both, and Err distinguishes them.
type Message struct {
	// Event is the payload under the envelope's "event" key: an executionReport, an
	// outboundAccountPosition, and so on, in the shape the normalizer takes. Nil when Err
	// is set.
	Event json.RawMessage

	// Err is non-nil only for a gap, and always wraps ErrGap.
	Err error
}

// StreamConfig is what a Stream needs. Every field that touches time or the network is
// injectable, so the tests run against a fake server and never against the live API.
type StreamConfig struct {
	// IntegrationID is whose weight budget the subscribe request is charged to (K24).
	IntegrationID uuid.UUID
	// Credential is the read-only key pair; Verify has already refused anything else (K9).
	Credential integration.Credential
	// URL defaults to SpotStreamURL. Injected so tests point at httptest.
	URL string
	// Limiter charges the subscribe request. Every reconnect re-subscribes, so a
	// reconnect loop spends weight and has to be visible to the shared IP budget.
	Limiter Limiter
	// Now defaults to time.Now. It is the signature timestamp and the gap boundary.
	Now func() time.Time
	// Backoff is the wait before reconnect attempt n (1-based). Defaults to the client's.
	Backoff func(attempt int) time.Duration
	// RecvWindow defaults to defaultRecvWindow.
	RecvWindow time.Duration
}

// Stream is one account's live user data feed. It owns a connection, re-establishes it when
// it drops, and reports the window it was down for.
//
// A dropped connection is routine rather than exceptional: Binance states a connection is
// valid for 24 hours and then disconnects, so a stream that does not reconnect stops
// working every day by design.
type Stream struct {
	cfg StreamConfig

	messages  chan Message
	closed    chan struct{}
	closeOnce sync.Once
	done      chan struct{}

	mu   sync.Mutex
	conn *websocket.Conn

	// cancel tears down every context-bound operation the run goroutine is inside -- a
	// dial, a subscribe read. Without it Close would still return, but the goroutine behind
	// it could sit in a thirty-second dial afterwards, which is a shutdown that has not
	// happened yet.
	cancel  context.CancelFunc
	started atomic.Bool

	connected        atomic.Bool
	unreadableFrames atomic.Int64
}

// NewStream validates the configuration and fills in the defaults. It opens nothing;
// Subscribe does that, so a caller that never subscribes holds no connection.
func NewStream(cfg StreamConfig) (*Stream, error) {
	if cfg.IntegrationID == uuid.Nil {
		return nil, errors.New("binance: stream needs an integration id to charge weight to")
	}
	if cfg.Credential.APIKey.Reveal() == "" || cfg.Credential.APISecret.Reveal() == "" {
		return nil, errors.New("binance: stream needs a credential")
	}
	if cfg.Limiter == nil {
		return nil, errors.New("binance: stream needs a limiter; every reconnect costs weight")
	}
	if cfg.URL == "" {
		cfg.URL = SpotStreamURL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Backoff == nil {
		cfg.Backoff = defaultBackoff
	}
	if cfg.RecvWindow <= 0 {
		cfg.RecvWindow = defaultRecvWindow
	}
	return &Stream{
		cfg:      cfg,
		messages: make(chan Message, 64),
		closed:   make(chan struct{}),
		done:     make(chan struct{}),
	}, nil
}

// Subscribe establishes the connection and returns the channel events arrive on. The first
// connect happens before it returns, so a rejected credential reaches the caller instead of
// disappearing into a retry loop.
//
// The returned channel is closed when the stream is closed. ctx bounds the stream's whole
// life, not just the first connect.
func (s *Stream) Subscribe(ctx context.Context) (<-chan Message, error) {
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
	return s.messages, nil
}

// Connected reports whether the stream is currently delivering. It answers the question a
// gap cannot: a gap is only reported once the connection is back, so a stream that has been
// down for an hour has emitted nothing at all. A supervisor reads this to go degraded.
func (s *Stream) Connected() bool { return s.connected.Load() }

// UnreadableFrames counts frames that could not be understood and were skipped. Exposed
// rather than logged because this package holds no logger; the supervisor logs it. It is
// counted at all because a stream silently dropping every frame looks exactly like a quiet
// account.
func (s *Stream) UnreadableFrames() int64 { return s.unreadableFrames.Load() }

// Close ends the stream and releases the connection. It is safe to call more than once.
//
// It waits for the reading goroutine to finish, so that when it returns the stream really
// has stopped -- the message channel is closed and nothing further will be written. A Close
// that returned while the goroutine was still inside an hour-long backoff would look like a
// clean shutdown and be a leak; every blocking point in that goroutine therefore watches
// either s.closed or the cancelled context.
func (s *Stream) Close() error {
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

// run reads frames until the connection fails, then reconnects and reports the gap. It is
// the only goroutine that writes to s.messages, so the channel is closed exactly once,
// here, on the way out.
func (s *Stream) run(ctx context.Context) {
	defer close(s.messages)
	defer close(s.done)
	defer s.connected.Store(false)

	for {
		s.readUntilFailure(ctx)
		s.connected.Store(false)

		// The moment the stream stopped delivering. Everything the account did between
		// here and the successful re-subscribe below is unknown to us.
		gapFrom := s.cfg.Now()

		conn, ok := s.reconnect(ctx)
		if !ok {
			return
		}
		s.setConn(conn)
		s.connected.Store(true)

		if !s.emit(Message{Err: &GapError{From: gapFrom, To: s.cfg.Now()}}) {
			return
		}
	}
}

// readUntilFailure delivers every event on the current connection and returns when it
// breaks. A frame it cannot read is counted and skipped: one malformed message must not end
// a stream that is otherwise healthy.
func (s *Stream) readUntilFailure(ctx context.Context) {
	for {
		conn := s.currentConn()
		if conn == nil {
			return
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		event, ok := unwrapEvent(data)
		if !ok {
			s.unreadableFrames.Add(1)
			continue
		}
		if !s.emit(Message{Event: event}) {
			return
		}
	}
}

// reconnect dials and re-subscribes with backoff, until it succeeds or the stream is
// closed. It reports false only when the stream is going away.
func (s *Stream) reconnect(ctx context.Context) (*websocket.Conn, bool) {
	for attempt := 1; ; attempt++ {
		if !s.wait(ctx, s.cfg.Backoff(attempt)) {
			return nil, false
		}
		conn, err := s.connect(ctx)
		if err == nil {
			return conn, true
		}
		// A rejected credential will not fix itself, but neither will refusing to retry:
		// the key may have been re-enabled, and the alternative is a stream that stays dead
		// with nobody watching. The backoff is what keeps the retry cheap.
		select {
		case <-s.closed:
			return nil, false
		case <-ctx.Done():
			return nil, false
		default:
		}
	}
}

// wait sleeps for d, or returns false as soon as the stream is closed. A Close that had to
// wait out a backoff would look like a hang on any interval long enough to be useful.
func (s *Stream) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-s.closed:
		return false
	case <-ctx.Done():
		return false
	}
}

// emit delivers one message, or reports false when the stream is closing. Without the
// closed check a shutdown with a full channel and no reader would block here forever.
func (s *Stream) emit(msg Message) bool {
	select {
	case s.messages <- msg:
		return true
	case <-s.closed:
		return false
	}
}

// connect dials the WebSocket API and subscribes on it. Both halves belong together: a
// connection without a subscription delivers nothing, and leaving one open would hold a
// session slot for no reason.
func (s *Stream) connect(ctx context.Context) (*websocket.Conn, error) {
	if err := s.cfg.Limiter.Acquire(
		ctx, s.cfg.IntegrationID, ratelimit.PriorityRealtime, weightSubscribe,
	); err != nil {
		return nil, fmt.Errorf("binance: subscribe weight: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, streamDialTimeout)
	defer cancel()
	// bodyclose is wrong here, and the suppression is narrow on purpose. Dial's own
	// documentation: "The response is the WebSocket handshake response from the server. You
	// never need to close resp.Body yourself." Closing it would be closing the hijacked
	// connection.
	conn, _, err := websocket.Dial(dialCtx, s.cfg.URL, nil) //nolint:bodyclose // see above
	if err != nil {
		return nil, fmt.Errorf("binance: dial user data stream: %w", err)
	}

	// The read limit guards against a frame large enough to exhaust memory. The default is
	// 32 KiB, which an outboundAccountPosition on an account holding many assets can pass.
	conn.SetReadLimit(4 << 20)

	if err := s.subscribe(ctx, conn); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	return conn, nil
}

// subscribe sends the signed subscription request and waits for its response.
func (s *Stream) subscribe(ctx context.Context, conn *websocket.Conn) error {
	requestID := uuid.NewString()
	params := s.signedParams()

	frame, err := json.Marshal(map[string]any{
		"id":     requestID,
		"method": subscribeMethod,
		"params": params,
	})
	if err != nil {
		return fmt.Errorf("binance: encode subscribe request: %w", err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, subscribeTimeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, frame); err != nil {
		return fmt.Errorf("binance: send subscribe request: %w", err)
	}

	// Read until the response to *this* request arrives. Events for an earlier subscription
	// can already be in flight on a reused session, and discarding one silently would be
	// the same data loss a gap reports.
	for {
		_, data, err := conn.Read(writeCtx)
		if err != nil {
			return fmt.Errorf("binance: await subscribe response: %w", err)
		}
		var response struct {
			ID     string `json:"id"`
			Status int    `json:"status"`
			Error  struct {
				Code int    `json:"code"`
				Msg  string `json:"msg"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &response); err != nil || response.ID != requestID {
			continue
		}
		if response.Status != 200 {
			// The venue's own words, and nothing else: no key, no secret, no signature
			// (L13).
			return fmt.Errorf("%w: status %d, code %d: %s",
				ErrSubscribeRejected, response.Status, response.Error.Code, response.Error.Msg)
		}
		return nil
	}
}

// signedParams builds the request params and signs them.
//
// The rule, from the request-security page on 2026-09-04: take every param except
// signature, sort them alphabetically by name, join them as name=value with "&", HMAC-SHA256
// the UTF-8 bytes under the secret, and hex encode.
//
// Note that this is not the REST rule. There the signed string is the URL-encoded query
// exactly as sent; here the values are joined raw. Signing one the other's way produces a
// valid signature for a request nobody made.
func (s *Stream) signedParams() map[string]any {
	values := map[string]string{
		"apiKey":     s.cfg.Credential.APIKey.Reveal(),
		"timestamp":  strconv.FormatInt(s.cfg.Now().UnixMilli(), 10),
		"recvWindow": strconv.FormatInt(s.cfg.RecvWindow.Milliseconds(), 10),
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+values[name])
	}

	params := make(map[string]any, len(values)+1)
	for name, value := range values {
		params[name] = value
	}
	// timestamp is a LONG in the documented parameter table, so it goes on the wire as a
	// number. The signed payload uses the same digits either way.
	params["timestamp"] = s.cfg.Now().UnixMilli()
	params["recvWindow"] = s.cfg.RecvWindow.Milliseconds()
	params["signature"] = Sign(s.cfg.Credential.APISecret.Reveal(), strings.Join(pairs, "&"))
	return params
}

// unwrapEvent peels the subscription envelope. Every user data event arrives as
// {"subscriptionId": N, "event": {...}}, one per text frame; the normalizer takes what is
// under "event", so the envelope is removed here, once, rather than in every caller.
//
// A frame with no "event" is not an error to raise -- it may be a response to a request we
// are no longer waiting for -- but it is not an event either, so it is reported as
// unreadable and counted.
func unwrapEvent(frame []byte) (json.RawMessage, bool) {
	var envelope struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return nil, false
	}
	if len(envelope.Event) == 0 {
		return nil, false
	}
	return envelope.Event, true
}

func (s *Stream) setConn(conn *websocket.Conn) {
	s.mu.Lock()
	previous := s.conn
	s.conn = conn
	s.mu.Unlock()
	if previous != nil {
		_ = previous.CloseNow()
	}
}

func (s *Stream) currentConn() *websocket.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}
