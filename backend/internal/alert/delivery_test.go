package alert_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/alert"
	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/stretchr/testify/require"
)

const botToken = "8199283746:AAH-THIS-IS-THE-SECRET-PART"

func message() alert.Message {
	return alert.Message{
		Title: "leverage above 3",
		Body:  "portfolio leverage is 4.1 (trigger 3)",
		Kind:  alert.Fired,
		At:    noon,
	}
}

// L13, and the trap this test exists for: the natural way to write a delivery failure puts
// the request URL in the error, and Telegram's URL CONTAINS THE TOKEN. Anyone with the token
// can message the user as us. The assertion is on the error string, because intent is not
// what leaks.
func TestAFailedTelegramDeliveryLeaksNeitherTokenNorURL(t *testing.T) {
	venue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
	}))
	defer venue.Close()

	tg := alert.Telegram{
		Token:   auth.Secret(botToken),
		ChatID:  "12345",
		BaseURL: venue.URL,
		Client:  venue.Client(),
	}

	err := tg.Deliver(context.Background(), message())
	require.Error(t, err)
	require.NotContains(t, err.Error(), botToken, "the bot token is in the error")
	require.NotContains(t, err.Error(), "AAH-THIS-IS-THE-SECRET-PART")
	require.NotContains(t, err.Error(), venue.URL,
		"the URL is in the error, and on the real venue the URL is where the token lives")
	require.Contains(t, err.Error(), "401", "an error that says nothing cannot be acted on")
}

// The same trap, one level up: a Telegram channel printed while debugging must not spill.
func TestATelegramChannelRedactsWhenPrinted(t *testing.T) {
	tg := alert.Telegram{Token: auth.Secret(botToken), ChatID: "12345"}
	for _, rendered := range []string{
		strings.TrimSpace(sprint("%v", tg)),
		strings.TrimSpace(sprint("%+v", tg)),
		strings.TrimSpace(sprint("%#v", tg)),
		strings.TrimSpace(sprint("%s", tg)),
	} {
		require.NotContains(t, rendered, botToken)
	}
}

func TestTelegramSendsToTheConfiguredChat(t *testing.T) {
	var gotPath, gotBody string
	venue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer venue.Close()

	tg := alert.Telegram{
		Token: auth.Secret(botToken), ChatID: "12345",
		BaseURL: venue.URL, Client: venue.Client(),
	}
	require.NoError(t, tg.Deliver(context.Background(), message()))
	require.Contains(t, gotPath, "/sendMessage")
	require.Contains(t, gotBody, "12345")
	require.Contains(t, gotBody, "leverage above 3")
}

// A webhook is the escape hatch that makes every other channel someone else's problem. It
// posts the alert as JSON, and its URL is a credential too: a webhook URL is a bearer token
// wearing a different hat.
func TestWebhookPostsTheAlertAsJSON(t *testing.T) {
	var body map[string]any
	venue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer venue.Close()

	hook := alert.Webhook{URL: auth.Secret(venue.URL), Client: venue.Client()}
	require.NoError(t, hook.Deliver(context.Background(), message()))
	require.Equal(t, "leverage above 3", body["title"])
	require.Equal(t, "fired", body["kind"])
}

func TestAFailedWebhookDeliveryLeaksNeitherURLNorBody(t *testing.T) {
	const secretPath = "/hooks/T00000/B11111/XXXXXXXXXXXXXXXXXXXXXXXX"
	venue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer venue.Close()

	hook := alert.Webhook{URL: auth.Secret(venue.URL + secretPath), Client: venue.Client()}
	err := hook.Deliver(context.Background(), message())
	require.Error(t, err)
	require.NotContains(t, err.Error(), secretPath,
		"a webhook URL is a bearer token wearing a different hat")
	require.Contains(t, err.Error(), "500")
}

// THE PATH THE REDACTION ACTUALLY LIVES ON.
//
// A venue that answers 401 never puts the URL in the error, because the status came back
// cleanly. The dangerous path is the one where the REQUEST fails: net/http wraps every
// transport error with the request URL, and Telegram's URL contains the token. The
// status-code tests above cannot reach this code at all -- a first pass proved it, by leaving
// two mutations alive that put the URL straight into the error.
func TestATransportFailureLeaksNeitherTokenNorURL(t *testing.T) {
	// A server that is closed before the request, so the connection is refused rather than
	// answered.
	venue := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := venue.URL
	client := venue.Client()
	venue.Close()

	tg := alert.Telegram{
		Token: auth.Secret(botToken), ChatID: "12345", BaseURL: dead, Client: client,
	}
	err := tg.Deliver(context.Background(), message())
	require.Error(t, err)
	require.NotContains(t, err.Error(), botToken, "the bot token reached the error")
	require.NotContains(t, err.Error(), dead,
		"the request URL reached the error, and on the real venue that URL holds the token")

	const secretPath = "/hooks/T00000/B11111/XXXXXXXXXXXXXXXXXXXXXXXX"
	hook := alert.Webhook{URL: auth.Secret(dead + secretPath), Client: client}
	err = hook.Deliver(context.Background(), message())
	require.Error(t, err)
	require.NotContains(t, err.Error(), secretPath,
		"the webhook URL reached the error; it is a bearer token wearing a different hat")
}

// A channel that hangs must not hang the evaluation behind it. The deadline is the caller's,
// so a delivery obeys the context it was given -- and says so without naming the request.
func TestDeliveryObeysItsContext(t *testing.T) {
	venue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer venue.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	hook := alert.Webhook{URL: auth.Secret(venue.URL), Client: venue.Client()}
	err := hook.Deliver(ctx, message())
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out",
		"a timeout must be distinguishable from a refusal: they have different fixes")
	require.NotContains(t, err.Error(), venue.URL)
}

// sprint keeps fmt out of the assertions above, where a stray verb would be the bug rather
// than the test.
func sprint(verb string, v any) string { return fmt.Sprintf(verb, v) }
