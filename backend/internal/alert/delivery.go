package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
)

// Message is what a channel is asked to deliver: already rendered, because a channel's job is
// transport and deciding what the user is told is the evaluator's.
type Message struct {
	Title string    `json:"title"`
	Body  string    `json:"body"`
	Kind  Kind      `json:"kind"`
	At    time.Time `json:"at"`
}

// Deliverer is one way of telling the user. An interface so the retry loop, the record and
// the redaction rules are written once, and a new channel is a new implementation rather than
// a new copy of all three.
type Deliverer interface {
	Deliver(ctx context.Context, m Message) error
}

// Telegram messages a chat through a bot.
//
// Token is auth.Secret and the URL is built inside Deliver, never stored on the struct,
// because Telegram puts the token IN THE PATH: anyone who reads a logged URL can message the
// user as us until the token is rotated (L13).
type Telegram struct {
	Token  auth.Secret
	ChatID string

	// BaseURL is https://api.telegram.org in production and a test server otherwise. Empty
	// means the real one.
	BaseURL string
	Client  *http.Client
}

// GoString redacts the token under %#v, which is the verb someone reaches for while
// debugging a struct and the one auth.Secret's Stringer cannot cover on its own.
func (t Telegram) GoString() string {
	return fmt.Sprintf("alert.Telegram{Token:REDACTED, ChatID:%q}", t.ChatID)
}

// Deliver sends the message to the configured chat.
//
// The error names the status and never the request: on the real venue the request URL is
// where the token lives, and an error is the one string that reliably reaches a log.
func (t Telegram) Deliver(ctx context.Context, m Message) error {
	base := t.BaseURL
	if base == "" {
		base = TelegramBaseURL
	}
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", base, url.PathEscape(t.Token.Reveal()))

	payload, err := json.Marshal(map[string]string{
		"chat_id": t.ChatID,
		"text":    m.Title + "\n" + m.Body,
	})
	if err != nil {
		return fmt.Errorf("alert: encode telegram message: %w", err)
	}

	status, err := post(ctx, t.Client, endpoint, payload)
	if err != nil {
		// The wrapped error from net/http carries the URL, so it is described rather than
		// wrapped: "%w" here would put the token in every log that prints this.
		return fmt.Errorf("alert: telegram delivery failed to reach the venue: %s",
			redactedCause(err))
	}
	if status >= 300 {
		return fmt.Errorf("alert: telegram refused the message with status %d", status)
	}
	return nil
}

// TelegramBaseURL is the venue. The token goes in the path after it, which is the whole
// reason this package is careful with URLs.
const TelegramBaseURL = "https://api.telegram.org"

// Webhook posts the alert as JSON to a URL the user supplied.
//
// The URL is auth.Secret because a webhook URL is a bearer token wearing a different hat:
// whoever holds it can post as the user's alerting system, forever, and it never appears in
// an error (L13).
type Webhook struct {
	URL    auth.Secret
	Client *http.Client
}

// GoString redacts the URL under %#v for the same reason Telegram's does.
func (w Webhook) GoString() string { return "alert.Webhook{URL:REDACTED}" }

// Deliver posts the alert as JSON. The URL never appears in the error, for the same reason
// Telegram's token does not.
func (w Webhook) Deliver(ctx context.Context, m Message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("alert: encode webhook payload: %w", err)
	}

	status, err := post(ctx, w.Client, w.URL.Reveal(), payload)
	if err != nil {
		return fmt.Errorf("alert: webhook delivery failed to reach the endpoint: %s",
			redactedCause(err))
	}
	if status >= 300 {
		return fmt.Errorf("alert: webhook endpoint refused the message with status %d", status)
	}
	return nil
}

// post is the one place either channel touches the network, so the timeout, the method and
// the content type cannot drift between them.
func post(ctx context.Context, client *http.Client, endpoint string, payload []byte) (int, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

// redactedCause turns a transport error into something safe to write down. net/http wraps
// every failure with the request URL, so the message itself is discarded and only the shape
// of the failure survives -- which is what an operator needs, and the whole of what is safe.
func redactedCause(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the request timed out"
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	default:
		return "the request could not be completed"
	}
}
