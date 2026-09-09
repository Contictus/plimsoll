package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// FeedID is the identity the market feed's weight is charged to.
//
// Prices belong to no account, but the limiter's budget is keyed per identity (K24), so the
// feed needs one of its own. A fixed UUID rather than a per-process random one: two workers
// on one host must share a bucket, because they share the IP the exchange is counting.
var FeedID = uuid.MustParse("00000000-0000-0000-0000-00000000feed")

// Documented weights (docs/BINANCE-API-NOTES.md F9). Named rather than inlined so the cost
// of a call is visible where the call is made.
const (
	// weightAllPrices is ticker/price with no symbol -- every symbol in one response.
	// Cheaper than asking for three symbols individually, which is what makes a full
	// snapshot on start affordable.
	weightAllPrices = 4
	weightKlines    = 2

	// klinesMaxLimit is the documented maximum. A little under 17 hours of minutes.
	klinesMaxLimit = 1000
)

// DefaultBaseURL is the public REST host. No key and no signature: every endpoint used here
// has security type NONE (F6).
const DefaultBaseURL = "https://api.binance.com"

// Limiter is the slice of the rate limiter this package uses. An interface so a test can
// assert the weight was charged without running a real budget.
type Limiter interface {
	Acquire(ctx context.Context, id uuid.UUID, priority ratelimit.Priority, weight int) error
}

// Client reads public market data. It holds no credential and signs nothing, which is not
// an omission -- it is the property that keeps this package out of the account path (F6).
type Client struct {
	baseURL string
	http    *http.Client
	limiter Limiter
}

// NewClient builds a client. baseURL is injected so tests run against httptest and never
// against the live API.
func NewClient(baseURL string, limiter Limiter, httpClient *http.Client) (*Client, error) {
	if limiter == nil {
		return nil, fmt.Errorf("marketdata: client needs a limiter; every call costs weight")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		// Never http.DefaultClient, which has no timeout and would hang a worker on a
		// stalled connection.
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: baseURL, http: httpClient, limiter: limiter}, nil
}

// Prices returns the last price of every symbol, as of now.
//
// This is what makes the stream usable at all. F7: the all-market stream carries only
// symbols that changed, so until a symbol trades the feed has nothing to say about it. A
// snapshot on start is therefore mandatory rather than an optimisation, and the same call
// refills after a disconnect.
func (c *Client) Prices(ctx context.Context) ([]Quote, error) {
	body, err := c.get(ctx, "/api/v3/ticker/price", nil, weightAllPrices)
	if err != nil {
		return nil, err
	}

	// Read as strings. A price through float64 comes back with different digits, and every
	// number derived from it is then wrong in a way no test on the derived number can see.
	var rows []struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("marketdata: decode ticker/price: %w", err)
	}

	// The snapshot has no per-symbol timestamp, so the observation time is the moment of
	// the response. That is honest: it is a statement about now, and dating it earlier
	// would make a fresh snapshot look stale while dating it later would hide real age.
	observedAt := time.Now().UTC()

	out := make([]Quote, 0, len(rows))
	for _, r := range rows {
		price, err := decimal.NewFromString(r.Price)
		if err != nil {
			return nil, fmt.Errorf("marketdata: %s price %q is not a decimal: %w",
				r.Symbol, r.Price, err)
		}
		out = append(out, Quote{Symbol: r.Symbol, Price: price, ObservedAt: observedAt})
	}
	return out, nil
}

// Klines returns one-minute closes for a symbol over a window, oldest first.
//
// It is how a gap is filled and how a past instant gets a real answer rather than a hole.
// The venue's own resolution here is exactly the resolution price_ticks stores (K7, F9), so
// a backfilled minute and a recorded minute are the same shape of row.
func (c *Client) Klines(ctx context.Context, symbol string, from, to time.Time, limit int) ([]Quote, error) {
	if limit <= 0 || limit > klinesMaxLimit {
		limit = klinesMaxLimit
	}
	params := url.Values{
		"symbol":    {symbol},
		"interval":  {"1m"},
		"startTime": {strconv.FormatInt(from.UTC().UnixMilli(), 10)},
		"endTime":   {strconv.FormatInt(to.UTC().UnixMilli(), 10)},
		"limit":     {strconv.Itoa(limit)},
	}
	body, err := c.get(ctx, "/api/v3/klines", params, weightKlines)
	if err != nil {
		return nil, err
	}

	// A kline is a positional array, which is why it is decoded as RawMessage and read by
	// index: index 0 is the open time and index 4 is the close price. A struct cannot
	// describe this, and a helper that guessed the positions would be wrong silently.
	var rows [][]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("marketdata: decode klines for %s: %w", symbol, err)
	}

	const (
		idxOpenTime   = 0
		idxClosePrice = 4
		minFields     = idxClosePrice + 1
	)

	out := make([]Quote, 0, len(rows))
	for i, row := range rows {
		if len(row) < minFields {
			return nil, fmt.Errorf("marketdata: %s kline %d has %d fields, need at least %d",
				symbol, i, len(row), minFields)
		}
		var openMillis int64
		if err := json.Unmarshal(row[idxOpenTime], &openMillis); err != nil {
			return nil, fmt.Errorf("marketdata: %s kline %d open time: %w", symbol, i, err)
		}
		var closePrice string
		if err := json.Unmarshal(row[idxClosePrice], &closePrice); err != nil {
			return nil, fmt.Errorf("marketdata: %s kline %d close price: %w", symbol, i, err)
		}
		price, err := decimal.NewFromString(closePrice)
		if err != nil {
			return nil, fmt.Errorf("marketdata: %s kline %d close %q is not a decimal: %w",
				symbol, i, closePrice, err)
		}

		// Klines are identified by their open time, and the close price is the price at the
		// END of that minute. Filing it under the open time is what makes it agree with the
		// live recorder, which files the last price seen during the minute under that same
		// minute. Filing it under the close time would shift every backfilled price one
		// minute into the future.
		out = append(out, Quote{
			Symbol: symbol, Price: price, ObservedAt: time.UnixMilli(openMillis).UTC(),
		})
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, params url.Values, weight int) ([]byte, error) {
	if err := c.limiter.Acquire(ctx, FeedID, ratelimit.PriorityBackfill, weight); err != nil {
		return nil, fmt.Errorf("marketdata: budget for %s: %w", path, err)
	}

	target := c.baseURL + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("marketdata: build request for %s: %w", path, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("marketdata: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("marketdata: read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("marketdata: %s answered %d: %s", path, resp.StatusCode, body)
	}
	return body, nil
}
