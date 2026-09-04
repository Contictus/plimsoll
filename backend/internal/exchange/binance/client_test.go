package binance_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/integration"
	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The secret every test signs with. Distinctive on purpose: several tests below grep
// output for it, and a value that could occur by chance would make those tests lie.
const (
	testAPIKey    = "PLIMSOLL-TEST-KEY-8fbc12"
	testAPISecret = "PLIMSOLL-TEST-SECRET-d41d8cd98f00b204e9800998ecf8427e"
)

type acquireCall struct {
	integrationID uuid.UUID
	priority      ratelimit.Priority
	weight        int
}

// fakeLimiter records what the client asked for. It never blocks: this file tests the
// client's use of the limiter, and the limiter's own behaviour is tested in its package.
type fakeLimiter struct {
	mu        sync.Mutex
	acquired  []acquireCall
	penalties []time.Duration
	observed  []int
	err       error
}

func (f *fakeLimiter) Acquire(
	_ context.Context, integrationID uuid.UUID, priority ratelimit.Priority, weight int,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, acquireCall{integrationID, priority, weight})
	return f.err
}

func (f *fakeLimiter) Penalize(retryAfter time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.penalties = append(f.penalties, retryAfter)
}

func (f *fakeLimiter) Observe(usedWeight int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = append(f.observed, usedWeight)
}

func (f *fakeLimiter) snapshot() ([]acquireCall, []time.Duration, []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]acquireCall(nil), f.acquired...),
		append([]time.Duration(nil), f.penalties...),
		append([]int(nil), f.observed...)
}

// newClient wires a client against handler. Backoff is zero so the retry tests assert the
// number of attempts without spending the wait.
func newClient(t *testing.T, handler http.HandlerFunc) (*binance.Client, *fakeLimiter, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	lim := &fakeLimiter{}
	client, err := binance.New(binance.Config{
		IntegrationID: uuid.New(),
		Credential: integration.Credential{
			APIKey:    auth.Secret(testAPIKey),
			APISecret: auth.Secret(testAPISecret),
		},
		Limiter: lim,
		BaseURL: srv.URL,
		Backoff: func(int) time.Duration { return 0 },
	})
	require.NoError(t, err)
	return client, lim, srv
}

// signHex is the test's own HMAC, deliberately written out rather than calling into the
// package: a helper shared with the implementation would agree with any bug it contained.
func signHex(secret, query string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(query))
	return hex.EncodeToString(mac.Sum(nil))
}

// The known-answer vector published by Binance, quoted from
// https://developers.binance.com/docs/binance-spot-api-docs/rest-api/endpoint-security-type
// and verified against that page on 2026-09-04.
//
// The query string is the documentation's own example and is used here purely as bytes to
// hash: no order endpoint is wrapped anywhere in this package, and none ever will be (L13).
// Its value is that the expected signature comes from Binance rather than from our own
// implementation, so this test cannot agree with a bug the way a round-trip test would.
func TestSignatureMatchesTheDocumentedVector(t *testing.T) {
	const (
		secret = "NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j"
		query  = "symbol=LTCBTC&side=BUY&type=LIMIT&timeInForce=GTC&quantity=1&price=0.1" +
			"&recvWindow=5000&timestamp=1499827319559"
		want = "c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71"
	)
	require.Equal(t, want, binance.Sign(secret, query))
}

// The signature must cover the exact query string that is sent. The server re-derives it
// from the bytes it actually received, so re-encoding or reordering between signing and
// sending fails here -- which is the whole failure mode, since Binance verifies the string
// it received and not the one we meant to send.
func TestSignedRequestCarriesKeyHeaderAndSignatureOverTheExactQuery(t *testing.T) {
	var (
		gotHeader string
		gotQuery  string
	)
	client, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-MBX-APIKEY")
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	_, err := client.Account(context.Background())
	require.NoError(t, err)

	require.Equal(t, testAPIKey, gotHeader, "the key travels in X-MBX-APIKEY")

	signed, sig, found := strings.Cut(gotQuery, "&signature=")
	require.True(t, found, "no signature parameter in %q", gotQuery)
	require.Equal(t, signHex(testAPISecret, signed), sig,
		"signature does not cover the query string as sent")
	require.Contains(t, signed, "timestamp=")
	require.Contains(t, signed, "recvWindow=")
}

// The exchange's own count is the authoritative one; ours is an approximation of it. A
// weight header that is read and dropped would let the two drift apart silently until the
// ban, which is the failure K24 exists to prevent.
func TestUsedWeightHeaderIsReportedToTheLimiter(t *testing.T) {
	client, lim, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-MBX-USED-WEIGHT-1M", "437")
		_, _ = w.Write([]byte(`{}`))
	})

	_, err := client.Account(context.Background())
	require.NoError(t, err)

	_, _, observed := lim.snapshot()
	require.Equal(t, []int{437}, observed)
}

func TestMissingWeightHeaderIsNotReportedAsZero(t *testing.T) {
	client, lim, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	_, err := client.Account(context.Background())
	require.NoError(t, err)

	_, _, observed := lim.snapshot()
	require.Empty(t, observed, "an absent header means unknown, not zero used weight")
}

// Weight is spent before the request leaves, and it is the endpoint's documented cost --
// not a per-call count. myTrades at 20 and a 1-weight call must not look alike.
func TestWeightIsAcquiredBeforeTheRequestWithTheEndpointsCost(t *testing.T) {
	var served int
	client, lim, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		served++
		_, _ = w.Write([]byte(`[]`))
	})

	_, err := client.MyTrades(context.Background(), binance.MyTradesQuery{Symbol: "BTCUSDT"})
	require.NoError(t, err)

	acquired, _, _ := lim.snapshot()
	require.Len(t, acquired, 1)
	require.Equal(t, 20, acquired[0].weight, "myTrades costs 20 (BINANCE-API-NOTES.md 2)")
	require.Equal(t, 1, served)
}

// A limiter that refuses must stop the request, not merely delay it. Sending anyway would
// make the limiter advisory, and an advisory rate limiter is decoration.
func TestNoRequestIsSentWhenTheLimiterRefuses(t *testing.T) {
	var served int
	client, lim, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		served++
		_, _ = w.Write([]byte(`{}`))
	})
	lim.err = errors.New("budget exhausted")

	_, err := client.Account(context.Background())
	require.Error(t, err)
	require.Zero(t, served, "the request must not be sent when weight was refused")
}

func TestPriorityIsPassedThroughAndOverridable(t *testing.T) {
	client, lim, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	_, err := client.Account(context.Background())
	require.NoError(t, err)
	_, err = client.WithPriority(ratelimit.PriorityBackfill).Account(context.Background())
	require.NoError(t, err)

	acquired, _, _ := lim.snapshot()
	require.Len(t, acquired, 2)
	require.Equal(t, ratelimit.PriorityRealtime, acquired[0].priority,
		"a client defaults to the priority a user is waiting on")
	require.Equal(t, ratelimit.PriorityBackfill, acquired[1].priority)
}

// 429 says a limit was broken and Retry-After says for how long. The duration has to reach
// the limiter: the next caller must be stopped by the penalty rather than discovering the
// same 429 for itself, because repeating it is what turns a 429 into a 418.
func TestRateLimitedCarriesRetryAfterAndPenalizes(t *testing.T) {
	var served int
	client, lim, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":-1003,"msg":"Too many requests."}`))
	})

	_, err := client.Account(context.Background())
	require.ErrorIs(t, err, binance.ErrRateLimited)

	var rl *binance.RateLimitError
	require.ErrorAs(t, err, &rl)
	require.Equal(t, 37*time.Second, rl.RetryAfter)
	require.False(t, rl.Banned)

	_, penalties, _ := lim.snapshot()
	require.Equal(t, []time.Duration{37 * time.Second}, penalties)
	require.Equal(t, 1, served, "a 429 must not be retried inside the call")
}

// 418 is an active IP ban, and bans escalate from two minutes to three days for continuing
// to send. Retrying is the single most expensive thing this client could do.
func TestBanIsReportedAndNeverRetried(t *testing.T) {
	var served int
	client, lim, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"code":-1003,"msg":"banned until 1499827319559"}`))
	})

	_, err := client.Account(context.Background())
	require.ErrorIs(t, err, binance.ErrBanned)
	require.NotErrorIs(t, err, binance.ErrRateLimited,
		"a ban is not a rate limit: the caller must not treat it as a short wait")

	var rl *binance.RateLimitError
	require.ErrorAs(t, err, &rl)
	require.True(t, rl.Banned)
	require.Equal(t, 120*time.Second, rl.RetryAfter)

	require.Equal(t, 1, served, "retrying a 418 extends the ban")
	_, penalties, _ := lim.snapshot()
	require.Equal(t, []time.Duration{120 * time.Second}, penalties)
}

// A 429 or 418 with no Retry-After still has to stop traffic. Falling through to "no
// penalty" would leave every other integration hammering an endpoint that has already
// started counting toward a ban.
func TestRateLimitWithoutRetryAfterStillPenalizes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"429 without Retry-After", http.StatusTooManyRequests},
		{"418 without Retry-After", http.StatusTeapot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, lim, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"code":-1003,"msg":"Too many requests."}`))
			})

			_, err := client.Account(context.Background())
			require.Error(t, err)

			_, penalties, _ := lim.snapshot()
			require.Len(t, penalties, 1)
			require.Greater(t, penalties[0], time.Duration(0),
				"an unstated Retry-After must become a conservative wait, not none")
		})
	}
}

// A 5xx is the exchange failing, not us being wrong. Retrying is correct, and it is the
// only status class where it is.
func TestServerErrorIsRetriedThenSucceeds(t *testing.T) {
	var served int
	client, _, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		served++
		if served < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"recovered":true}`))
	})

	body, err := client.Account(context.Background())
	require.NoError(t, err)
	require.JSONEq(t, `{"recovered":true}`, string(body))
	require.Equal(t, 3, served)
}

// A retry is a second request at the exchange and costs its weight there whether or not
// we counted it. Charging only the first attempt would let a 5xx burst spend up to
// MaxAttempts times the weight our budget believes it spent -- an undercount of the shared
// IP budget, which is the one that ends in a ban (K24).
func TestEveryAttemptChargesItsOwnWeight(t *testing.T) {
	var served int
	client, lim, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		served++
		if served < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})

	_, err := client.Account(context.Background())
	require.NoError(t, err)

	acquired, _, _ := lim.snapshot()
	require.Len(t, acquired, served,
		"weight must be charged once per request sent, not once per call")
	for i, call := range acquired {
		require.Equal(t, 20, call.weight, "attempt %d charged the wrong weight", i+1)
	}
}

func TestServerErrorGivesUpAfterTheAttemptBudget(t *testing.T) {
	var served int
	client, _, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":-1000,"msg":"An unknown error occurred."}`))
	})

	_, err := client.Account(context.Background())
	require.Error(t, err)
	require.Equal(t, binance.MaxAttempts, served)
}

// A 4xx means the request was wrong. Sending it again spends weight to be told so twice.
func TestClientErrorIsNotRetried(t *testing.T) {
	var served int
	client, _, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":-1121,"msg":"Invalid symbol."}`))
	})

	_, err := client.Account(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, served)

	var apiErr *binance.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, -1121, apiErr.Code)
	require.Equal(t, "Invalid symbol.", apiErr.Msg)
	require.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
}

// K9 rejects an over-permissioned key at connect time, but a key can also lose a
// permission afterwards. That has to be its own error: it is fixed by re-issuing the key,
// not by retrying or by waiting.
func TestPermissionFailureIsItsOwnError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			client, _, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`))
			})

			_, err := client.Account(context.Background())
			require.ErrorIs(t, err, binance.ErrPermission)
		})
	}
}

// L13: the secret must not reach an error, however the caller formats it. Every failure
// path is driven and every verb checked, because the one that leaks is the one nobody
// thought to look at.
func TestNoErrorEverContainsTheSecret(t *testing.T) {
	handlers := map[string]http.HandlerFunc{
		"429": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
		},
		"418": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		},
		"401": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":-2015,"msg":"Invalid API-key."}`))
		},
		"400": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":-1121,"msg":"Invalid symbol."}`))
		},
		"500": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"garbage body": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`<html>gateway error</html>`))
		},
	}

	for name, handler := range handlers {
		t.Run(name, func(t *testing.T) {
			client, _, _ := newClient(t, handler)
			_, err := client.Account(context.Background())
			require.Error(t, err)

			// Every verb, because %v and %s go through Error but %#v does not, and a
			// struct field holding the credential would surface only under %#v.
			for _, verb := range []string{"%v", "%+v", "%s", "%#v", "%q"} {
				rendered := fmt.Sprintf(verb, err)
				require.NotContains(t, rendered, testAPISecret, "verb %s leaked the secret", verb)
				require.NotContains(t, rendered, testAPIKey, "verb %s leaked the api key", verb)
			}
		})
	}
}

// The signature is derived from the secret. It is not the secret, but it is minted from it
// on every call, so it has no business in an error either.
func TestErrorsDoNotEchoTheSignedQuery(t *testing.T) {
	client, _, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":-1121,"msg":"Invalid symbol."}`))
	})

	_, err := client.Account(context.Background())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "signature=")
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	var served int
	ctx, cancel := context.WithCancel(context.Background())
	client, _, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		served++
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := client.Account(ctx)
	require.Error(t, err)
	require.Equal(t, 1, served, "a cancelled context must not fund another attempt")
}

// ExchangeInfo is unsigned: it carries no account data, and signing it would put a
// signature in the logs of every connect.
func TestExchangeInfoIsUnsigned(t *testing.T) {
	var gotQuery, gotHeader string
	client, _, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotHeader = r.URL.RawQuery, r.Header.Get("X-MBX-APIKEY")
		_, _ = w.Write([]byte(`{"rateLimits":[]}`))
	})

	_, err := client.ExchangeInfo(context.Background())
	require.NoError(t, err)
	require.NotContains(t, gotQuery, "signature=")
	require.Empty(t, gotHeader, "an unsigned endpoint must not present the key")
}

// The per-minute ceiling is read from the exchange, never hardcoded (K24): a number we
// compile in is a number that is wrong the day Binance changes it, and we find out from
// the ban.
func TestRequestWeightCeilingIsReadFromExchangeInfo(t *testing.T) {
	raw := json.RawMessage(`{"rateLimits":[
	  {"rateLimitType":"REQUEST_WEIGHT","interval":"MINUTE","intervalNum":1,"limit":6000},
	  {"rateLimitType":"RAW_REQUESTS","interval":"MINUTE","intervalNum":5,"limit":61000},
	  {"rateLimitType":"ORDERS","interval":"SECOND","intervalNum":10,"limit":100}
	]}`)

	got, err := binance.RequestWeightPerMinute(raw)
	require.NoError(t, err)
	require.Equal(t, 6000, got)
}

// A window that is not one minute cannot be divided down: REQUEST_WEIGHT over 5 minutes is
// not 1/5 of the burst, and guessing produces a ceiling that is wrong in the direction
// that gets us banned.
func TestMissingRequestWeightCeilingIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"no rateLimits at all", `{}`},
		{"no REQUEST_WEIGHT entry", `{"rateLimits":[{"rateLimitType":"ORDERS","interval":"SECOND","intervalNum":10,"limit":100}]}`},
		{"REQUEST_WEIGHT over a different window", `{"rateLimits":[{"rateLimitType":"REQUEST_WEIGHT","interval":"MINUTE","intervalNum":5,"limit":6000}]}`},
		{"not an object", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := binance.RequestWeightPerMinute(json.RawMessage(tc.raw))
			require.Error(t, err)
		})
	}
}
