package binance_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/integration"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var streamNow = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

// subscribeRequest is the frame the stream sends, as the documentation shapes it.
type subscribeRequest struct {
	ID     string            `json:"id"`
	Method string            `json:"method"`
	Params map[string]any    `json:"params"`
	Raw    map[string]any    `json:"-"`
	Order  []json.RawMessage `json:"-"`
}

// fakeWSServer is a Binance WebSocket API that exists only in memory. It records every
// subscribe request, answers it, and lets a test drop the connection underneath the stream.
type fakeWSServer struct {
	t *testing.T

	mu         sync.Mutex
	subscribes []subscribeRequest
	conns      int

	// status is what the server answers a subscribe with. 200 unless a test says otherwise.
	status int

	// dropAfter closes the connection after this many events have been sent on it. Zero
	// leaves it open.
	dropAfter int

	// frames is what the server sends after a successful subscribe, in order. Each is a
	// complete text frame.
	frames []string

	srv *httptest.Server
}

func newFakeWSServer(t *testing.T, frames ...string) *fakeWSServer {
	t.Helper()
	f := &fakeWSServer{t: t, status: 200, frames: frames}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWSServer) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	ctx := r.Context()
	f.mu.Lock()
	f.conns++
	status := f.status
	dropAfter := f.dropAfter
	frames := append([]string(nil), f.frames...)
	f.mu.Unlock()

	_, data, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var req subscribeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}
	f.mu.Lock()
	f.subscribes = append(f.subscribes, req)
	f.mu.Unlock()

	var response string
	if status == 200 {
		response = fmt.Sprintf(
			`{"id":%q,"status":200,"result":{"subscriptionId":0}}`, req.ID)
	} else {
		response = fmt.Sprintf(
			`{"id":%q,"status":%d,"error":{"code":-2014,"msg":"API-key format invalid."}}`,
			req.ID, status)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(response)); err != nil {
		return
	}

	for i, frame := range frames {
		if dropAfter > 0 && i >= dropAfter {
			_ = conn.CloseNow()
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			return
		}
	}
	<-ctx.Done()
}

func (f *fakeWSServer) requests() []subscribeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]subscribeRequest(nil), f.subscribes...)
}

func (f *fakeWSServer) connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func (f *fakeWSServer) stopDropping() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropAfter = 0
}

func newStream(t *testing.T, f *fakeWSServer) *binance.Stream {
	t.Helper()
	s, err := binance.NewStream(binance.StreamConfig{
		IntegrationID: uuid.New(),
		Credential: integration.Credential{
			APIKey:    auth.Secret(testAPIKey),
			APISecret: auth.Secret(testAPISecret),
		},
		URL:     f.srv.URL,
		Limiter: &fakeLimiter{},
		Now:     func() time.Time { return streamNow },
		Backoff: func(int) time.Duration { return 5 * time.Millisecond },
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// wantEvent reads the next event off the stream, failing rather than hanging if none comes.
func wantEvent(t *testing.T, messages <-chan binance.Message) binance.Message {
	t.Helper()
	select {
	case msg, ok := <-messages:
		require.True(t, ok, "the stream closed before delivering an event")
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no message arrived")
		return binance.Message{}
	}
}

// tradeFrame is the frame the fake server sends: the recorded fixture, verbatim, envelope
// and all. Hand-writing a frame here would test the stream against my memory of the
// envelope; sending the fixture tests it against the documented shape the normalizer
// already reads.
func tradeFrame(t *testing.T) string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "testdata", "fixtures", "binance",
		"execution_report_trade_bnbbtc.json"))
	require.NoError(t, err)
	return string(blob)
}

// The signature is checked against the rule the documentation states, computed here with
// the standard library rather than by calling binance.Sign -- a round trip through our own
// signer would agree with any bug it contains.
//
// The rule, quoted from the request-security page on 2026-09-04: take all request params
// except `signature`, sort them alphabetically by name, join them as `name=value` with `&`,
// HMAC-SHA256 the UTF-8 bytes under the secret key, and hex encode the result.
func TestTheSubscribeRequestIsSignedAsTheDocumentationStates(t *testing.T) {
	f := newFakeWSServer(t, tradeFrame(t))
	s := newStream(t, f)

	messages, err := s.Subscribe(context.Background())
	require.NoError(t, err)
	wantEvent(t, messages)

	requests := f.requests()
	require.Len(t, requests, 1)
	req := requests[0]

	require.Equal(t, "userDataStream.subscribe.signature", req.Method)
	require.NotEmpty(t, req.ID, "a request without an id cannot be matched to its response")
	require.Equal(t, testAPIKey, req.Params["apiKey"])

	signature, ok := req.Params["signature"].(string)
	require.True(t, ok, "the request carries no signature")

	var pairs []string
	for name, value := range req.Params {
		if name == "signature" {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%s=%v", name, formatParam(value)))
	}
	sort.Strings(pairs)

	mac := hmac.New(sha256.New, []byte(testAPISecret))
	mac.Write([]byte(strings.Join(pairs, "&")))
	require.Equal(t, hex.EncodeToString(mac.Sum(nil)), signature,
		"payload signed was %q", strings.Join(pairs, "&"))
}

// formatParam renders a decoded JSON value the way it appeared on the wire. Numbers decode
// to float64, and a timestamp printed in scientific notation would sign a different string
// than the one sent.
func formatParam(value any) string {
	if number, ok := value.(float64); ok {
		return fmt.Sprintf("%d", int64(number))
	}
	return fmt.Sprintf("%v", value)
}

// Events arrive wrapped: {"subscriptionId": N, "event": {...}}. The consumer is the
// normalizer, which expects the payload Binance documents -- so the envelope is peeled
// here, once, rather than in every caller.
func TestEventsAreDeliveredUnwrappedAndUsableByTheNormalizer(t *testing.T) {
	f := newFakeWSServer(t, tradeFrame(t))
	s := newStream(t, f)

	messages, err := s.Subscribe(context.Background())
	require.NoError(t, err)

	msg := wantEvent(t, messages)
	require.NoError(t, msg.Err)

	event, err := binance.NormalizeStreamExecutionReport(
		context.Background(), resolverFor("BNBBTC", 7), binance.IngestContext{
			AccountID:     uuid.New(),
			IntegrationID: uuid.New(),
			Source:        binance.SourceStream,
		}, msg.Event)
	require.NoError(t, err, "the delivered payload must be exactly what the normalizer takes")
	require.Equal(t, binance.SpotTradeID("BNBBTC", 28457), event.VenueEventID,
		"the stream and the REST walk must produce one identity for one trade (L5)")
}

// A dropped connection is normal: Binance states a connection is valid for 24 hours and
// then disconnects, so a stream that does not reconnect stops working every day by design.
func TestADroppedConnectionReconnectsAndResubscribes(t *testing.T) {
	f := newFakeWSServer(t, tradeFrame(t), tradeFrame(t))
	f.dropAfter = 1
	s := newStream(t, f)

	messages, err := s.Subscribe(context.Background())
	require.NoError(t, err)
	require.NoError(t, wantEvent(t, messages).Err)

	f.stopDropping()

	// The reconnect reports its gap, then events resume.
	deadline := time.After(10 * time.Second)
	sawEvent := false
	for !sawEvent {
		select {
		case msg := <-messages:
			if msg.Err == nil {
				sawEvent = true
			}
		case <-deadline:
			t.Fatal("the stream never delivered another event after the drop")
		}
	}
	require.GreaterOrEqual(t, f.connections(), 2, "the stream must have dialled again")
	require.GreaterOrEqual(t, len(f.requests()), 2, "a new connection must re-subscribe")
}

// Nothing in the protocol says what happened while we were disconnected, so the window has
// to be reported rather than assumed empty. The supervisor turns it into a bounded REST
// resync; without it the events in that window are simply lost, silently (L11).
func TestAReconnectReportsTheWindowItWasDisconnected(t *testing.T) {
	f := newFakeWSServer(t, tradeFrame(t), tradeFrame(t))
	f.dropAfter = 1

	// A moving clock, so the reported window is a real interval rather than an instant. A
	// gap that always came back empty would be indistinguishable from no gap at all, and
	// the resync it drives would read nothing.
	var tick atomic.Int64
	s, err := binance.NewStream(binance.StreamConfig{
		IntegrationID: uuid.New(),
		Credential: integration.Credential{
			APIKey:    auth.Secret(testAPIKey),
			APISecret: auth.Secret(testAPISecret),
		},
		URL:     f.srv.URL,
		Limiter: &fakeLimiter{},
		Now:     func() time.Time { return streamNow.Add(time.Duration(tick.Add(1)) * time.Second) },
		Backoff: func(int) time.Duration { return 5 * time.Millisecond },
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	messages, err := s.Subscribe(context.Background())
	require.NoError(t, err)
	require.NoError(t, wantEvent(t, messages).Err)
	f.stopDropping()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-messages:
			if msg.Err == nil {
				continue
			}
			require.ErrorIs(t, msg.Err, binance.ErrGap)

			var gap *binance.GapError
			require.ErrorAs(t, msg.Err, &gap)
			require.False(t, gap.From.IsZero(), "a gap with no start cannot be resynced")
			require.False(t, gap.To.IsZero(), "a gap with no end cannot be resynced")
			require.True(t, gap.To.After(gap.From),
				"the window must span the outage, not collapse to an instant")
			return
		case <-deadline:
			t.Fatal("the reconnect never reported a gap")
		}
	}
}

// A frame we cannot read is skipped, because one malformed message must not end the
// stream. It is counted, because a stream silently dropping every frame looks exactly like
// a quiet account -- which is the failure L11 exists to prevent.
func TestAnUnreadableFrameIsSkippedAndCounted(t *testing.T) {
	f := newFakeWSServer(t,
		`{"this is not json`,
		`{"subscriptionId":0}`,
		tradeFrame(t),
	)
	s := newStream(t, f)

	messages, err := s.Subscribe(context.Background())
	require.NoError(t, err)

	msg := wantEvent(t, messages)
	require.NoError(t, msg.Err)
	require.Contains(t, string(msg.Event), "executionReport",
		"the good frame must survive the two bad ones before it")
	require.EqualValues(t, 2, s.UnreadableFrames(),
		"both bad frames must be counted, not merely survived")
}

// Close during a backoff must not wait the backoff out. A worker shutting down while its
// stream is retrying would otherwise hang for the whole interval, which on a long backoff
// is long enough to look like a hang.
func TestCloseDuringBackoffReturnsPromptly(t *testing.T) {
	f := newFakeWSServer(t, tradeFrame(t))
	f.dropAfter = 1

	s, err := binance.NewStream(binance.StreamConfig{
		IntegrationID: uuid.New(),
		Credential: integration.Credential{
			APIKey:    auth.Secret(testAPIKey),
			APISecret: auth.Secret(testAPISecret),
		},
		URL:     f.srv.URL,
		Limiter: &fakeLimiter{},
		Now:     func() time.Time { return streamNow },
		Backoff: func(int) time.Duration { return time.Hour },
	})
	require.NoError(t, err)

	messages, err := s.Subscribe(context.Background())
	require.NoError(t, err)
	require.NoError(t, wantEvent(t, messages).Err)

	// The connection has now dropped and the stream is inside its hour-long backoff.
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for the backoff instead of interrupting it")
	}

	// Close returning is only worth something if the stream actually stopped. Close waits
	// for the reading goroutine, and that goroutine closes the channel on its way out, so a
	// closed channel here is the proof that the backoff was interrupted rather than merely
	// abandoned.
	select {
	case _, ok := <-messages:
		require.False(t, ok, "the message channel must be closed once Close returns")
	default:
		t.Fatal("the stream is still running after Close returned")
	}
}

// A rejected key is not a transport failure. Retrying it forever would hide a credential
// problem behind a reconnect loop and spend weight doing it, so Subscribe reports it to the
// caller -- without putting the credential in the error (L13).
func TestARejectedSubscribeIsReportedAndNeverEchoesTheCredential(t *testing.T) {
	f := newFakeWSServer(t)
	f.status = 401
	s := newStream(t, f)

	_, err := s.Subscribe(context.Background())
	require.Error(t, err)
	require.NotContains(t, err.Error(), testAPISecret)
	require.NotContains(t, err.Error(), testAPIKey)
	require.Contains(t, err.Error(), "-2014", "the error must name what the venue said")
}
