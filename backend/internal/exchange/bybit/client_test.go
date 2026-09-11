package bybit_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/exchange/bybit"
	"github.com/Contictus/plimsoll/backend/internal/integration"
	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type countingLimiter struct{ spent int }

func (l *countingLimiter) Acquire(
	_ context.Context, _ uuid.UUID, weight int, _ ratelimit.Priority,
) error {
	l.spent += weight
	return nil
}

const (
	testKey    = "test-api-key"
	testSecret = "test-api-secret"
)

var pinned = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

func newClient(t *testing.T, srv *httptest.Server, limiter bybit.Limiter) *bybit.Client {
	t.Helper()
	c, err := bybit.New(bybit.Config{
		IntegrationID: uuid.New(),
		Credential: integration.Credential{
			APIKey:    auth.Secret(testKey),
			APISecret: auth.Secret(testSecret),
		},
		Limiter: limiter,
		BaseURL: srv.URL,
		Now:     func() time.Time { return pinned },
	})
	require.NoError(t, err)
	return c
}

// The signature is over `timestamp + api_key + recv_window + queryString`, in that order
// (B1). This is not Binance's scheme, and getting it wrong produces a valid-looking
// signature over the wrong string -- which fails only against the live venue, at the one
// moment there is no way to debug it cheaply.
//
// The test recomputes the expected value independently rather than calling the code under
// test, so it is checking the scheme and not the implementation's opinion of the scheme.
func TestTheSignatureIsOverTimestampKeyRecvWindowAndQuery(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(`{"retCode":0,"retMsg":"OK","result":{}}`))
	}))
	defer srv.Close()

	_, err := newClient(t, srv, &countingLimiter{}).QueryAPI(context.Background())
	require.NoError(t, err)

	timestamp := strconv.FormatInt(pinned.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(timestamp + testKey + "5000" + got.URL.RawQuery))
	want := hex.EncodeToString(mac.Sum(nil))

	require.Equal(t, want, got.Header.Get("X-BAPI-SIGN"))
	require.Equal(t, testKey, got.Header.Get("X-BAPI-API-KEY"))
	require.Equal(t, timestamp, got.Header.Get("X-BAPI-TIMESTAMP"))
	require.Equal(t, "5000", got.Header.Get("X-BAPI-RECV-WINDOW"))
}

// A business failure arrives as HTTP 200 with a non-zero retCode.
//
// A client that only checked the status would read "your key expired" as a successful empty
// page -- and a backfill would record a complete history of nothing, with its cursor advanced
// past the gap. This is the single most important difference from the Binance adapter.
func TestANonZeroRetCodeIsAFailureDespiteTheTwoHundred(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"retCode":10003,"retMsg":"API key is invalid","result":{}}`))
	}))
	defer srv.Close()

	_, err := newClient(t, srv, &countingLimiter{}).QueryAPI(context.Background())
	require.ErrorIs(t, err, bybit.ErrVenue)
	require.Contains(t, err.Error(), "10003")
	require.NotContains(t, err.Error(), testSecret, "the credential is never in an error (L13)")
}

// Budget is spent before the request leaves, not after. Asking a rate limiter for
// forgiveness is asking the whole IP to wait (K24).
func TestBudgetIsSpentBeforeTheRequestLeaves(t *testing.T) {
	limiter := &countingLimiter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.Positive(t, limiter.spent, "the budget must already be spent when we arrive")
		_, _ = w.Write([]byte(`{"retCode":0,"retMsg":"OK","result":{}}`))
	}))
	defer srv.Close()

	_, err := newClient(t, srv, limiter).QueryAPI(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, limiter.spent)
}

// A refused key is not retried. A retry answers the same refusal one budget unit later, and
// four of them is four.
func TestARefusedKeyIsNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newClient(t, srv, &countingLimiter{}).QueryAPI(context.Background())
	require.ErrorIs(t, err, bybit.ErrPermission)
	require.Equal(t, 1, calls)
}

// A 5xx is retried, because it is the venue being down rather than us being wrong.
func TestAServerErrorIsRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"retCode":0,"retMsg":"OK","result":{}}`))
	}))
	defer srv.Close()

	c, err := bybit.New(bybit.Config{
		IntegrationID: uuid.New(),
		Credential: integration.Credential{
			APIKey: auth.Secret(testKey), APISecret: auth.Secret(testSecret),
		},
		Limiter: &countingLimiter{},
		BaseURL: srv.URL,
		Now:     func() time.Time { return pinned },
		Backoff: func(int) time.Duration { return time.Millisecond },
	})
	require.NoError(t, err)

	_, err = c.QueryAPI(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, calls)
}

// A client with no limiter is refused at construction. Unlimited requests get the IP banned,
// and the ban is shared by every account on it (K24).
func TestAClientWithoutALimiterIsRefused(t *testing.T) {
	_, err := bybit.New(bybit.Config{IntegrationID: uuid.New(), BaseURL: "https://example.test"})
	require.Error(t, err)
}
