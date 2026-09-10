//go:build integration

package httpapi_test

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/stretchr/testify/require"
)

// openStream connects to an SSE endpoint and returns a reader over its lines. The response
// never ends, so nothing here may call decodeJSON or io.ReadAll.
func openStream(t *testing.T, url string, cookie *http.Cookie) *bufio.Reader {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the cleanup below
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	return bufio.NewReader(resp.Body)
}

// nextEvent reads until an SSE data line arrives, or fails. Comment lines (the heartbeat) are
// skipped, which is what a real client does too.
func nextEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	done := make(chan string, 1)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				done <- strings.TrimSpace(strings.TrimPrefix(line, "data: "))
				return
			}
		}
	}()
	select {
	case data := <-done:
		return data
	case <-time.After(10 * time.Second):
		t.Fatal("no event arrived on the stream")
		return ""
	}
}

// The stream carries a HINT and never the numbers (K51). A client is told "positions changed"
// and re-reads through the endpoint it already uses -- so there is one description of what a
// position is, one freshness envelope, and one place where tenancy is decided.
func TestTheStreamCarriesAHintNotTheNumbers(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("stream-hint"))
	accountID := accountOf(t, srv, cookie)

	stream := openStream(t, srv.URL+"/stream/positions", cookie)

	// The fold is what publishes, so this is the worker's own path rather than a test hook.
	integrationID, instrumentID, symbol, _ := seedFoldedPosition(t, accountID)
	appendFill(t, accountID, integrationID, instrumentID, "buy", "1", "100", 21)
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	data := nextEvent(t, stream)
	require.Contains(t, data, `"topic":"positions"`)
	require.NotContains(t, data, symbol, "the stream is carrying position data")
	require.NotContains(t, data, "quantity")
}

// An unauthenticated client gets the same 401 every other endpoint gives, and no stream.
func TestAStreamRequiresASession(t *testing.T) {
	srv := newServer(t)
	resp := do(t, http.MethodGet, srv.URL+"/stream/portfolio", nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// One account's fold must not wake another account's stream. The channel is per account, so
// this is a property of the transport rather than of a filter someone could forget.
func TestAnotherAccountsChangeDoesNotWakeThisStream(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)

	watcher := register(t, srv, uniqueEmail("stream-watcher"))
	stream := openStream(t, srv.URL+"/stream/positions", watcher)

	stranger := register(t, srv, uniqueEmail("stream-stranger"))
	strangerID := accountOf(t, srv, stranger)
	integrationID, instrumentID, _, _ := seedFoldedPosition(t, strangerID)
	appendFill(t, strangerID, integrationID, instrumentID, "buy", "1", "100", 22)
	_, err := projection.Project(ctx, appPool(t), strangerID, integrationID)
	require.NoError(t, err)

	arrived := make(chan string, 1)
	go func() {
		for {
			line, err := stream.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				arrived <- line
				return
			}
		}
	}()
	select {
	case line := <-arrived:
		t.Fatalf("another account's change woke this stream: %s", line)
	case <-time.After(2 * time.Second):
	}
}

// A client watching one topic is not woken by another. The fold publishes `positions` and
// `portfolio`; a risk page must stay quiet through it, or every fill re-reads every screen
// the user has open and the topics are decoration.
func TestAStreamIsNotWokenByAnotherTopic(t *testing.T) {
	ctx := context.Background()
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("stream-topic"))
	accountID := accountOf(t, srv, cookie)

	stream := openStream(t, srv.URL+"/stream/risk", cookie)

	integrationID, instrumentID, _, _ := seedFoldedPosition(t, accountID)
	appendFill(t, accountID, integrationID, instrumentID, "buy", "1", "100", 23)
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	arrived := make(chan string, 1)
	go func() {
		for {
			line, err := stream.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				arrived <- line
				return
			}
		}
	}()
	select {
	case line := <-arrived:
		t.Fatalf("a positions change was delivered on the risk stream: %s", line)
	case <-time.After(2 * time.Second):
	}
}
