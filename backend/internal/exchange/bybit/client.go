// Package bybit is the REST adapter for Bybit V5. It signs requests, spends budget through
// the shared limiter before anything leaves the process, and hands back the raw payload --
// decoding belongs to the normalizer, and the raw bytes are what L15 keeps forever.
//
// No order endpoint is wrapped here, and none ever will be: not unused, not behind a flag,
// not commented out (L13). The only signed endpoints in this package read.
//
// Every fact it encodes -- the signature payload, the base URL, the window and page limits --
// is verified in `docs/BYBIT-API-NOTES.md` and dated. Nothing here is remembered.
package bybit

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/integration"
	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/google/uuid"
)

var (
	// ErrRateLimited is the venue asking us to slow down. Recoverable.
	ErrRateLimited = errors.New("bybit: rate limited")

	// ErrPermission means the key was rejected rather than the request. K9 refuses an
	// over-permissioned key at connect time, but a key can also lose access later; that is
	// fixed by re-issuing it, never by retrying.
	ErrPermission = errors.New("bybit: api key rejected or lacking permission")

	// ErrVenue is a business error: HTTP 200 with a non-zero retCode. Bybit reports failure
	// in the body rather than the status, so a client that only checked the status would
	// treat "your key expired" as a successful empty page -- and a backfill would record a
	// complete history of nothing.
	ErrVenue = errors.New("bybit: request refused")
)

const (
	// MaxAttempts bounds one call including retries. Only 5xx is retried.
	MaxAttempts = 4

	// defaultRecvWindow is Bybit's own default, sent explicitly so the value is visible in
	// the signed string rather than being whatever the server assumes (B1).
	defaultRecvWindow = 5 * time.Second

	// unstatedPenalty is the wait applied when the venue asks us to slow down without
	// saying for how long.
	unstatedPenalty = time.Minute

	// requestCost is what one call spends in the shared limiter.
	//
	// Bybit does not publish a per-endpoint weight for the two endpoints this package uses
	// (B3), so a cost is chosen rather than read. One is the smallest honest answer: it
	// counts the call without claiming to know what the venue charges for it, and the
	// shared per-IP gate (K24) is what actually protects the address either way.
	requestCost = 1
)

// Header names, quoted from the guide (B1).
const (
	headerAPIKey     = "X-BAPI-API-KEY"
	headerTimestamp  = "X-BAPI-TIMESTAMP"
	headerSign       = "X-BAPI-SIGN"
	headerRecvWindow = "X-BAPI-RECV-WINDOW"
)

// Limiter is the slice of the rate limiter this package needs.
type Limiter interface {
	Acquire(ctx context.Context, integrationID uuid.UUID, weight int, p ratelimit.Priority) error
}

// Config builds a Client.
type Config struct {
	IntegrationID uuid.UUID
	Credential    integration.Credential
	Limiter       Limiter

	// BaseURL has no trailing slash, e.g. https://api.bybit.com. Injected so tests run
	// against httptest and never against the live API.
	BaseURL string

	HTTPClient *http.Client
	Now        func() time.Time
	Backoff    func(attempt int) time.Duration
	RecvWindow time.Duration
}

// Client is one integration's REST connection to Bybit. Safe for concurrent use: everything
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

// New builds a Client at realtime priority. A backfill asks for its lower priority through
// WithPriority, so the patient caller declares itself rather than the urgent one.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.IntegrationID == uuid.Nil:
		return nil, errors.New("bybit: config needs an integration id to charge against")
	case cfg.Limiter == nil:
		return nil, errors.New("bybit: config needs a limiter; unlimited requests get the ip banned")
	case cfg.BaseURL == "":
		return nil, errors.New("bybit: config needs a base url")
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

// WithPriority returns a copy whose calls queue at p.
func (c *Client) WithPriority(p ratelimit.Priority) *Client {
	clone := *c
	clone.priority = p
	return &clone
}

func defaultBackoff(attempt int) time.Duration {
	return time.Duration(1<<(attempt-1)) * 500 * time.Millisecond
}
