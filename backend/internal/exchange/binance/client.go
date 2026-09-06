// Package binance is the REST adapter for Binance spot. It signs requests, spends weight
// through the rate limiter before anything leaves the process, and hands back the raw
// payload -- decoding belongs to the normalizer, and the raw bytes are what L15 keeps
// forever.
//
// No order endpoint is wrapped here, and none ever will be: not unused, not behind a
// flag, not commented out (L13). The only signed endpoints in this package read.
package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/integration"
	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/google/uuid"
)

var (
	// ErrRateLimited is a 429: a limit was broken and Binance says how long to wait. It is
	// recoverable, and the wait is the exchange's number rather than a guess of ours.
	ErrRateLimited = errors.New("binance: rate limited")

	// ErrBanned is a 418: the IP is already banned for continuing to send after a 429.
	// Deliberately not an ErrRateLimited, because the two need opposite handling -- bans
	// escalate from two minutes to three days, and the escalation is driven by retrying.
	ErrBanned = errors.New("binance: ip banned")

	// ErrPermission means the key was rejected rather than the request. K9 refuses an
	// over-permissioned key at connect time, but a key can also lose the read permission
	// later; that is fixed by re-issuing the key, never by retrying.
	ErrPermission = errors.New("binance: api key rejected or lacking permission")
)

const (
	// MaxAttempts bounds one call including retries. Only 5xx is retried, so this is a
	// budget for the exchange being down, not for us being wrong.
	MaxAttempts = 4

	// defaultRecvWindow is Binance's own default. Sent explicitly so the value is visible
	// in the signed string rather than being whatever the server assumes.
	defaultRecvWindow = 5 * time.Second

	// unstatedPenalty is the wait applied when a 429 or 418 arrives with no Retry-After.
	// A minute is longer than any legitimate weight window, so the guess errs toward
	// waiting too long -- the other direction is a three-day ban.
	unstatedPenalty = time.Minute

	// usedWeightHeader is the per-minute counter Binance returns on every request, quoted
	// from the docs as X-MBX-USED-WEIGHT-(intervalNum)(intervalLetter)
	// (docs/BINANCE-API-NOTES.md 3).
	usedWeightHeader = "X-MBX-USED-WEIGHT-1M"

	// apiKeyHeader is where the key travels, quoted from the endpoint security docs.
	apiKeyHeader = "X-MBX-APIKEY"

	// maxErrorBody bounds how much of an unparsable response reaches an error message. A
	// gateway's HTML page is not worth an unbounded log line.
	maxErrorBody = 200
)

// Limiter is the slice of the rate limiter this client needs. Declared here rather than
// imported as a concrete type so the client can be tested without a real budget, and so
// the dependency points one way: ratelimit knows nothing about HTTP.
type Limiter interface {
	// Acquire blocks until weight may be spent, or returns an error if it never may.
	Acquire(ctx context.Context, integrationID uuid.UUID, priority ratelimit.Priority, weight int) error
	// Penalize stops all traffic from this IP for retryAfter.
	Penalize(retryAfter time.Duration)
	// Observe reports the exchange's own used-weight count, which is the authoritative
	// one; ours only approximates it.
	Observe(usedWeight int)
}

// Config is everything a Client needs. Every field except the defaults is required, and
// New says which one is missing rather than failing at the first request.
type Config struct {
	// IntegrationID is whose budget the calls are charged to (K24).
	IntegrationID uuid.UUID
	// Credential is the read-only key pair. Verify has already refused anything else (K9).
	Credential integration.Credential
	Limiter    Limiter
	// BaseURL has no trailing slash, e.g. https://api.binance.com. Injected so tests run
	// against httptest and never against the live API.
	BaseURL string

	// HTTPClient defaults to a client with a timeout. Never http.DefaultClient, which has
	// none and would hang a worker on a stalled connection.
	HTTPClient *http.Client
	// Now defaults to time.Now. It is the signature timestamp, so a test can pin it.
	Now func() time.Time
	// Backoff is the wait before retry attempt n (1-based). Defaults to exponential.
	Backoff func(attempt int) time.Duration
	// RecvWindow defaults to defaultRecvWindow.
	RecvWindow time.Duration
}

// Client is one integration's REST connection. It is safe for concurrent use: everything
// it holds is read-only after New, and the mutable state lives in the limiter.
type Client struct {
	integrationID uuid.UUID
	cred          integration.Credential
	limiter       Limiter
	baseURL       string
	http          *http.Client
	now           func() time.Time
	backoff       func(int) time.Duration
	recvWindow    time.Duration
	priority      ratelimit.Priority
}

// New builds a Client. The returned client is realtime priority: a backfill has to ask for
// its lower priority explicitly through WithPriority, so the patient caller is the one
// that declares itself rather than the urgent one.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.IntegrationID == uuid.Nil:
		return nil, errors.New("binance: config needs an integration id to charge weight to")
	case cfg.Limiter == nil:
		return nil, errors.New("binance: config needs a limiter; unlimited requests get the ip banned")
	case cfg.BaseURL == "":
		return nil, errors.New("binance: config needs a base url")
	}

	c := &Client{
		integrationID: cfg.IntegrationID,
		cred:          cfg.Credential,
		limiter:       cfg.Limiter,
		baseURL:       strings.TrimRight(cfg.BaseURL, "/"),
		http:          cfg.HTTPClient,
		now:           cfg.Now,
		backoff:       cfg.Backoff,
		recvWindow:    cfg.RecvWindow,
		priority:      ratelimit.PriorityRealtime,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.backoff == nil {
		c.backoff = defaultBackoff
	}
	if c.recvWindow == 0 {
		c.recvWindow = defaultRecvWindow
	}
	return c, nil
}

// WithPriority returns a copy of the client whose calls queue at p. A copy rather than a
// setter, so a backfill cannot change the priority of the realtime client it was cloned
// from halfway through a request.
func (c *Client) WithPriority(p ratelimit.Priority) *Client {
	clone := *c
	clone.priority = p
	return &clone
}

// defaultBackoff waits 500ms, 1s, 2s. Only 5xx reaches it, and a 5xx that outlasts three
// and a half seconds is an outage rather than a blip -- the caller is better served by an
// error it can report than by a call that never returns.
func defaultBackoff(attempt int) time.Duration {
	return 500 * time.Millisecond * (1 << (attempt - 1))
}

// Sign is HMAC-SHA256 of query under secret, hex encoded. Exported so it can be checked
// against Binance's published vector rather than against our own output.
//
// The caller must sign the exact string it sends: Binance verifies the query it received,
// so re-encoding or reordering between signing and sending produces a valid signature for
// a request nobody made.
func Sign(secret, query string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(query))
	return hex.EncodeToString(mac.Sum(nil))
}

// request is one endpoint call before it is signed.
type request struct {
	path   string
	query  url.Values
	weight int
	signed bool
}

// APIError is Binance's own error body. Code is its numeric error code, which is the field
// worth branching on -- the HTTP status collapses many distinct causes into 400.
type APIError struct {
	StatusCode int
	Code       int
	Msg        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("binance: http %d: code %d: %s", e.StatusCode, e.Code, e.Msg)
}

// Unwrap maps the statuses that mean "the key was rejected" onto ErrPermission, so a
// caller can branch on the cause without knowing Binance's numbering.
func (e *APIError) Unwrap() error {
	if e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden {
		return ErrPermission
	}
	return nil
}

// RateLimitError carries how long to wait. Banned separates a 418 from a 429 because they
// need opposite handling, and conflating them is how a two-minute ban becomes a three-day
// one.
type RateLimitError struct {
	RetryAfter time.Duration
	Banned     bool
}

func (e *RateLimitError) Error() string {
	if e.Banned {
		return fmt.Sprintf("binance: ip banned, retry after %s", e.RetryAfter)
	}
	return fmt.Sprintf("binance: rate limited, retry after %s", e.RetryAfter)
}

func (e *RateLimitError) Unwrap() error {
	if e.Banned {
		return ErrBanned
	}
	return ErrRateLimited
}

// retryable marks a failure worth sending again. It never escapes the package: a caller
// deciding to retry for itself is how a 418 gets extended.
type retryable struct{ err error }

func (r retryable) Error() string { return r.err.Error() }
func (r retryable) Unwrap() error { return r.err }

// do spends weight and performs req, retrying only what is worth retrying. It returns the
// response body untouched (L15).
func (c *Client) do(ctx context.Context, req request) (json.RawMessage, error) {
	for attempt := 1; ; attempt++ {
		// Charged per attempt, not per call: a retry is a second request at the exchange
		// and costs its weight there whatever we think.
		if err := c.limiter.Acquire(ctx, c.integrationID, c.priority, req.weight); err != nil {
			return nil, fmt.Errorf("binance: %s: acquire %d weight: %w", req.path, req.weight, err)
		}

		body, err := c.attempt(ctx, req)
		if err == nil {
			return body, nil
		}

		var again retryable
		if !errors.As(err, &again) || attempt >= MaxAttempts {
			return nil, err
		}
		// Checked after the attempt rather than before: a cancelled caller must not fund
		// another request, and the wait below would otherwise report the cancellation as
		// a timeout.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("binance: %s: %w", req.path, ctxErr)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("binance: %s: %w", req.path, ctx.Err())
		case <-time.After(c.backoff(attempt)):
		}
	}
}

// attempt performs exactly one HTTP request. Errors from here never contain the credential
// or the signed query (L13): the path and the status are enough to debug with.
func (c *Client) attempt(ctx context.Context, req request) (json.RawMessage, error) {
	target, err := c.url(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		// target holds the signature, so it is deliberately not in the message.
		return nil, fmt.Errorf("binance: %s: build request: %w", req.path, err)
	}
	if req.signed {
		httpReq.Header.Set(apiKeyHeader, c.cred.APIKey.Reveal())
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A transport failure is the exchange or the network, not the request: retryable.
		// url.Error would echo the signed query, so it is replaced rather than wrapped.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("binance: %s: %w", req.path, ctxErr)
		}
		return nil, retryable{fmt.Errorf("binance: %s: request failed", req.path)}
	}
	defer func() { _ = resp.Body.Close() }()

	// Read before branching on status: the counter is returned on failures too, and a 429
	// is exactly when knowing the real number matters most.
	c.reportUsedWeight(resp.Header)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, retryable{fmt.Errorf("binance: %s: read body: %w", req.path, err)}
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusTeapot:
		return nil, c.rateLimited(resp)
	case resp.StatusCode >= 500:
		return nil, retryable{apiError(resp.StatusCode, body)}
	case resp.StatusCode >= 400:
		return nil, apiError(resp.StatusCode, body)
	}
	return json.RawMessage(body), nil
}

// rateLimited turns a 429 or 418 into a penalty on the whole IP. The penalty is applied
// here rather than left to the caller because the 429 was earned by the IP: every other
// integration has to stop too, and one that is told nothing keeps sending (K24).
func (c *Client) rateLimited(resp *http.Response) error {
	wait := unstatedPenalty
	if header := resp.Header.Get("Retry-After"); header != "" {
		if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
			wait = time.Duration(seconds) * time.Second
		}
	}
	c.limiter.Penalize(wait)
	return &RateLimitError{RetryAfter: wait, Banned: resp.StatusCode == http.StatusTeapot}
}

// reportUsedWeight forwards the exchange's counter. An absent header means unknown, and
// unknown is not zero: reporting zero would tell the limiter the budget had just reset.
func (c *Client) reportUsedWeight(header http.Header) {
	raw := header.Get(usedWeightHeader)
	if raw == "" {
		return
	}
	used, err := strconv.Atoi(raw)
	if err != nil || used < 0 {
		return
	}
	c.limiter.Observe(used)
}

// apiError decodes Binance's error body, falling back to a truncated excerpt when the body
// is not the JSON we expect -- an HTML gateway page must still produce a usable error.
func apiError(status int, body []byte) error {
	var decoded struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &decoded); err == nil && decoded.Msg != "" {
		return &APIError{StatusCode: status, Code: decoded.Code, Msg: decoded.Msg}
	}
	excerpt := strings.TrimSpace(string(body))
	if len(excerpt) > maxErrorBody {
		excerpt = excerpt[:maxErrorBody] + "..."
	}
	if excerpt == "" {
		excerpt = "empty response body"
	}
	return &APIError{StatusCode: status, Msg: excerpt}
}

// url builds the full request URL, signing when the endpoint requires it. The signature
// covers the encoded query exactly as it is about to be sent, and is appended afterwards
// so it can never be reordered into a different string than the one that was signed.
func (c *Client) url(req request) (string, error) {
	query := url.Values{}
	for key, values := range req.query {
		query[key] = values
	}

	if !req.signed {
		return c.baseURL + req.path + suffix(query.Encode()), nil
	}

	query.Set("recvWindow", strconv.FormatInt(c.recvWindow.Milliseconds(), 10))
	query.Set("timestamp", strconv.FormatInt(c.now().UnixMilli(), 10))

	encoded := query.Encode()
	return c.baseURL + req.path + "?" + encoded + "&signature=" + Sign(c.cred.APISecret.Reveal(), encoded), nil
}

func suffix(encoded string) string {
	if encoded == "" {
		return ""
	}
	return "?" + encoded
}
