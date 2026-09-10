package alert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
)

// The channel kinds this build can deliver through.
const (
	KindTelegram = "telegram"
	KindWebhook  = "webhook"
)

// ErrUnknownChannelKind means a stored row names a transport this build cannot speak.
var ErrUnknownChannelKind = errors.New("alert: unknown channel kind")

// Channel is one configured destination, as a listing sees it: what it is called and whether
// it is on. It deliberately carries NOTHING that could be used to send -- an endpoint that
// listed channels and returned the token would hand the account's alerting to anyone who
// could read one response.
type Channel struct {
	ID        uuid.UUID
	Kind      string
	Label     string
	Enabled   bool
	CreatedAt time.Time
}

// channelConfig is the shape that gets sealed. Never returned, never logged, and never
// marshalled anywhere but into the ciphertext.
type channelConfig struct {
	Token  string `json:"token,omitempty"`
	ChatID string `json:"chat_id,omitempty"`
	URL    string `json:"url,omitempty"`
}

// StoreTelegram seals a bot token and the chat it messages. q must come from tenancy.InTx.
func StoreTelegram(
	ctx context.Context, q *store.Queries, kp crypto.KeyProvider,
	accountID uuid.UUID, label string, token auth.Secret, chatID string,
) (uuid.UUID, error) {
	return storeChannel(ctx, q, kp, accountID, KindTelegram, label,
		channelConfig{Token: token.Reveal(), ChatID: chatID})
}

// StoreWebhook seals a webhook URL, which is a credential for the same reason a token is.
func StoreWebhook(
	ctx context.Context, q *store.Queries, kp crypto.KeyProvider,
	accountID uuid.UUID, label string, endpoint auth.Secret,
) (uuid.UUID, error) {
	return storeChannel(ctx, q, kp, accountID, KindWebhook, label,
		channelConfig{URL: endpoint.Reveal()})
}

func storeChannel(
	ctx context.Context, q *store.Queries, kp crypto.KeyProvider,
	accountID uuid.UUID, kind, label string, cfg channelConfig,
) (uuid.UUID, error) {
	plaintext, err := json.Marshal(cfg)
	if err != nil {
		// The error names neither the config nor its fields: the encoder is the only thing
		// that could have failed, and everything else here is a secret.
		return uuid.Nil, fmt.Errorf("alert: encode %s channel config: %w", kind, err)
	}
	ciphertext, wrapped, version, err := crypto.Seal(ctx, kp, plaintext)
	if err != nil {
		return uuid.Nil, fmt.Errorf("alert: seal %s channel for %s: %w", kind, accountID, err)
	}
	id, err := q.CreateAlertChannel(ctx, store.CreateAlertChannelParams{
		AccountID: accountID, Kind: kind, Label: label, Enabled: true,
		ConfigCiphertext: ciphertext, WrappedDek: wrapped, KeyVersion: int32(version), //nolint:gosec // a key version
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("alert: store %s channel for %s: %w", kind, accountID, err)
	}
	return id, nil
}

// ListChannels is the listing an endpoint can serve: labels and kinds, never secrets.
func ListChannels(
	ctx context.Context, q *store.Queries, accountID uuid.UUID,
) ([]Channel, error) {
	rows, err := q.ListAlertChannels(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("alert: read channels for %s: %w", accountID, err)
	}
	out := make([]Channel, 0, len(rows))
	for _, r := range rows {
		out = append(out, Channel{
			ID: r.ID, Kind: r.Kind, Label: r.Label,
			Enabled: r.Enabled, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// Deliverers decrypts the enabled channels into things that can send.
//
// Only the worker calls this, and only immediately before delivering: a decrypted token has a
// lifetime, and its lifetime should be one evaluation pass rather than the process's.
func Deliverers(
	ctx context.Context, q *store.Queries, kp crypto.KeyProvider,
	accountID uuid.UUID, client *http.Client,
) ([]Deliverer, error) {
	rows, err := q.ListAlertChannels(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("alert: read channels for %s: %w", accountID, err)
	}

	out := make([]Deliverer, 0, len(rows))
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		plaintext, err := crypto.Open(ctx, kp, r.ConfigCiphertext, r.WrappedDek, int(r.KeyVersion))
		if err != nil {
			// Named by channel id, never by label content or config (L13).
			return nil, fmt.Errorf("alert: open channel %s: %w", r.ID, err)
		}
		var cfg channelConfig
		if err := json.Unmarshal(plaintext, &cfg); err != nil {
			return nil, fmt.Errorf("alert: decode channel %s: %w", r.ID, err)
		}

		switch r.Kind {
		case KindTelegram:
			out = append(out, Telegram{
				Token: auth.Secret(cfg.Token), ChatID: cfg.ChatID, Client: client,
			})
		case KindWebhook:
			out = append(out, Webhook{URL: auth.Secret(cfg.URL), Client: client})
		default:
			return nil, fmt.Errorf("%w: channel %s is %q", ErrUnknownChannelKind, r.ID, r.Kind)
		}
	}
	return out, nil
}
