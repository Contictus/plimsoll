package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/alert"
	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Thresholds are money paths like any other (L1): a leverage limit parsed as a float is a
// limit rounded in the digits it was set in.

type alertRuleBody struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Metric string    `json:"metric" enum:"leverage,net_leverage,gross_exposure,margin_buffer,liquidation_distance,concentration"`

	ScopeKind string `json:"scope_kind" enum:"portfolio,strategy,position"`
	ScopeName string `json:"scope_name" doc:"the strategy or position; empty for the portfolio"`

	Comparator string `json:"comparator" enum:"above,below"`
	Trigger    string `json:"trigger"    doc:"where the condition starts"`
	Clear      string `json:"clear"      doc:"where it ends -- a different number, or the rule fires on every evaluation"`

	CooldownSeconds int  `json:"cooldown_seconds"`
	Enabled         bool `json:"enabled"`

	// Firing is what the evaluator currently believes, so a client can render the rule and
	// its state without a second call.
	Firing      bool       `json:"firing"`
	Since       *time.Time `json:"since"`
	LastFiredAt *time.Time `json:"last_fired_at"`
}

type alertRulesBody struct {
	Rules []alertRuleBody `json:"rules"`
}

type alertBody struct {
	ID       uuid.UUID `json:"id"`
	RuleID   uuid.UUID `json:"rule_id"`
	RuleName string    `json:"rule_name"`

	Kind      string `json:"kind" enum:"fired,resolved,unavailable"`
	Metric    string `json:"metric"`
	ScopeKind string `json:"scope_kind"`
	ScopeName string `json:"scope_name"`

	// Value is empty for `unavailable`: a metric nobody could compute and a metric that is
	// zero are opposite claims (L11).
	Value string `json:"value"`

	// RunID names the valuation this was decided from, so an alert can be traced to the
	// prices behind it -- the same claim the rest of this system makes about its numbers.
	RunID *int64 `json:"run_id"`

	FiredAt     time.Time  `json:"fired_at"`
	DeliveredAt *time.Time `json:"delivered_at"`

	// DeliveryError is redacted at its source: the channels never put a token or a URL in an
	// error, which is what makes it safe to serve one here (L13).
	DeliveryError string `json:"delivery_error"`
}

type alertsBody struct {
	Alerts []alertBody `json:"alerts"`
}

// channelBody names a channel and reveals nothing that could be used to send through it. A
// listing that returned the token would hand the account's alerting to anyone who could read
// one response.
type channelBody struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind" enum:"telegram,webhook"`
	Label     string    `json:"label"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

type channelsBody struct {
	Channels []channelBody `json:"channels"`
}

type createRuleInput struct {
	Body struct {
		Name            string `json:"name" minLength:"1" maxLength:"80"`
		Metric          string `json:"metric" enum:"leverage,net_leverage,gross_exposure,margin_buffer,liquidation_distance,concentration"`
		ScopeKind       string `json:"scope_kind" enum:"portfolio,strategy,position" required:"false"`
		ScopeName       string `json:"scope_name" required:"false" doc:"the strategy or position; omit for the portfolio"`
		Comparator      string `json:"comparator" enum:"above,below"`
		Trigger         string `json:"trigger"`
		Clear           string `json:"clear"`
		CooldownSeconds int    `json:"cooldown_seconds" required:"false"`
	}
}

type updateRuleInput struct {
	ID   string `path:"id"`
	Body struct {
		Name            string `json:"name" minLength:"1" maxLength:"80"`
		Comparator      string `json:"comparator" enum:"above,below"`
		Trigger         string `json:"trigger"`
		Clear           string `json:"clear"`
		CooldownSeconds int    `json:"cooldown_seconds" required:"false"`
		Enabled         bool   `json:"enabled"`
	}
}

// createChannelInput carries a secret in a request body, which is the one direction a
// credential may travel: in. It is sealed before it is stored and never comes back out (L13).
type createChannelInput struct {
	Body struct {
		Kind  string `json:"kind" enum:"telegram,webhook"`
		Label string `json:"label" minLength:"1" maxLength:"80"`

		// Each kind needs its own fields, so none of them is required by the schema and all
		// of them are checked by the handler -- which is the layer that knows which kind is
		// being created, and can say so in a sentence.
		Token  string `json:"token"   required:"false" doc:"telegram bot token; write-only"`
		ChatID string `json:"chat_id" required:"false" doc:"telegram chat id"`
		URL    string `json:"url"     required:"false" doc:"webhook endpoint; write-only, and a credential"`
	}
}

func (d Deps) registerAlerts(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-alert-rules",
		Method:      http.MethodGet,
		Path:        "/alert-rules",
		Summary:     "The thresholds this account asked to be told about",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body alertRulesBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		var rows []store.ListAlertRulesRow
		if err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			rows, err = q.ListAlertRules(ctx, accountID)
			return err
		}); err != nil {
			return nil, err
		}

		out := make([]alertRuleBody, 0, len(rows))
		for _, r := range rows {
			body := alertRuleBody{
				ID: r.ID, Name: r.Name, Metric: r.Metric,
				ScopeKind: r.ScopeKind, ScopeName: r.ScopeName,
				Comparator: r.Comparator,
				Trigger:    r.TriggerAt.String(), Clear: r.ClearAt.String(),
				CooldownSeconds: int(r.CooldownSeconds), Enabled: r.Enabled,
				Since: r.Since, LastFiredAt: r.LastFiredAt,
			}
			if r.Firing != nil {
				body.Firing = *r.Firing
			}
			out = append(out, body)
		}
		return &struct{ Body alertRulesBody }{Body: alertRulesBody{Rules: out}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "create-alert-rule",
		Method:      http.MethodPost,
		Path:        "/alert-rules",
		Summary:     "Ask to be told when a number crosses a line",
		Description: "`trigger` and `clear` are two different numbers on purpose. One" +
			" threshold approached from both sides makes a metric sitting on it fire at" +
			" every evaluation, and a user messaged twenty times silences the channel.",
	}, func(ctx context.Context, in *createRuleInput) (*struct {
		Body struct {
			ID uuid.UUID `json:"id"`
		}
	}, error,
	) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		trigger, err := decimal.NewFromString(in.Body.Trigger)
		if err != nil {
			return nil, huma.Error400BadRequest("trigger must be a number, as a string")
		}
		clearAt, err := decimal.NewFromString(in.Body.Clear)
		if err != nil {
			return nil, huma.Error400BadRequest("clear must be a number, as a string")
		}
		// Checked here as well as in the schema, so the caller gets a sentence rather than a
		// constraint name. The schema is what makes it true; this is what makes it kind.
		if (in.Body.Comparator == string(alert.Above) && clearAt.GreaterThan(trigger)) ||
			(in.Body.Comparator == string(alert.Below) && clearAt.LessThan(trigger)) {
			return nil, huma.Error400BadRequest(
				"the clear threshold is on the wrong side of the trigger: an 'above' rule" +
					" clears below its trigger and a 'below' rule clears above it")
		}

		scopeKind := in.Body.ScopeKind
		if scopeKind == "" {
			scopeKind = alert.ScopePortfolio
		}
		var id uuid.UUID
		err = tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			id, err = q.CreateAlertRule(ctx, store.CreateAlertRuleParams{
				AccountID: accountID, Name: in.Body.Name, Metric: in.Body.Metric,
				ScopeKind: scopeKind, ScopeName: in.Body.ScopeName,
				Comparator: in.Body.Comparator, TriggerAt: trigger, ClearAt: clearAt,
				CooldownSeconds: int32(in.Body.CooldownSeconds), //nolint:gosec // bounded by the caller's own request
				Enabled:         true,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		out := &struct {
			Body struct {
				ID uuid.UUID `json:"id"`
			}
		}{}
		out.Body.ID = id
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-alert-rule",
		Method:      http.MethodPut,
		Path:        "/alert-rules/{id}",
		Summary:     "Retune a threshold, or switch it off",
		Description: "Both thresholds are replaced together: a trigger changed without its" +
			" clear line is how a band ends up pointing the wrong way.",
	}, func(ctx context.Context, in *updateRuleInput) (*struct{}, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, huma.Error400BadRequest("malformed rule id")
		}
		trigger, err := decimal.NewFromString(in.Body.Trigger)
		if err != nil {
			return nil, huma.Error400BadRequest("trigger must be a number, as a string")
		}
		clearAt, err := decimal.NewFromString(in.Body.Clear)
		if err != nil {
			return nil, huma.Error400BadRequest("clear must be a number, as a string")
		}

		var rows int64
		err = tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			rows, err = q.UpdateAlertRule(ctx, store.UpdateAlertRuleParams{
				AccountID: accountID, ID: id, Name: in.Body.Name,
				Comparator: in.Body.Comparator, TriggerAt: trigger, ClearAt: clearAt,
				CooldownSeconds: int32(in.Body.CooldownSeconds), //nolint:gosec // bounded by the caller's own request
				Enabled:         in.Body.Enabled,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		if rows == 0 {
			// An UPDATE that matched no row is otherwise silent success, and the user would
			// believe they had retuned a rule that is still watching the old line.
			return nil, huma.Error404NotFound("no such rule")
		}
		return &struct{}{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-alerts",
		Method:      http.MethodGet,
		Path:        "/alerts",
		Summary:     "What has been said, and whether it was delivered",
		Description: "The record exists whether or not the message left the building:" +
			" delivery is transport, and an account whose channel expired still has a" +
			" history of what happened while nobody was being told.",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body alertsBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		var rows []store.ListAlertsRow
		if err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			rows, err = q.ListAlerts(ctx, store.ListAlertsParams{
				AccountID: accountID, MaxRows: alertPageSize,
			})
			return err
		}); err != nil {
			return nil, err
		}

		out := make([]alertBody, 0, len(rows))
		for _, a := range rows {
			body := alertBody{
				ID: a.ID, RuleID: a.RuleID, RuleName: a.RuleName, Kind: a.Kind,
				Metric: a.Metric, ScopeKind: a.ScopeKind, ScopeName: a.ScopeName,
				Value: nullText(a.Value), RunID: a.RunID,
				FiredAt: a.FiredAt, DeliveredAt: a.DeliveredAt,
			}
			if a.DeliveryError != nil {
				body.DeliveryError = *a.DeliveryError
			}
			out = append(out, body)
		}
		return &struct{ Body alertsBody }{Body: alertsBody{Alerts: out}}, nil
	})

	d.registerAlertChannels(api)
}

// alertPageSize is what one screen of history is. Not configurable, because the alert list is
// a dashboard panel rather than an export.
const alertPageSize = 100

// registerAlertChannels is where secrets enter this system, and the only place they do.
//
// They travel in and never out: there is no endpoint that returns a token or a webhook URL,
// not redacted and not partially. A "show me what I configured" affordance is worth less than
// the failure mode it opens, because a channel secret is a standing permission to speak as
// the user's alerting system.
func (d Deps) registerAlertChannels(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-alert-channels",
		Method:      http.MethodGet,
		Path:        "/alert-channels",
		Summary:     "Where alerts are sent",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body channelsBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		var channels []alert.Channel
		if err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			channels, err = alert.ListChannels(ctx, q, accountID)
			return err
		}); err != nil {
			return nil, err
		}

		out := make([]channelBody, 0, len(channels))
		for _, c := range channels {
			out = append(out, channelBody{
				ID: c.ID, Kind: c.Kind, Label: c.Label,
				Enabled: c.Enabled, CreatedAt: c.CreatedAt,
			})
		}
		return &struct{ Body channelsBody }{Body: channelsBody{Channels: out}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "create-alert-channel",
		Method:      http.MethodPost,
		Path:        "/alert-channels",
		Summary:     "Add somewhere for alerts to go",
		Description: "The token or URL is envelope-encrypted on arrival and is never" +
			" returned by any endpoint. A webhook URL is a credential too: whoever holds it" +
			" can post as this account's alerting system.",
	}, func(ctx context.Context, in *createChannelInput) (*struct {
		Body struct {
			ID uuid.UUID `json:"id"`
		}
	}, error,
	) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		if d.Keys == nil {
			// Refused rather than stored in the clear. A process without a key provider
			// cannot keep this safe, and storing it anyway is the failure L13 forbids.
			return nil, huma.Error500InternalServerError("this process cannot store secrets")
		}

		var id uuid.UUID
		err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			switch in.Body.Kind {
			case alert.KindTelegram:
				if in.Body.Token == "" || in.Body.ChatID == "" {
					return huma.Error400BadRequest("a telegram channel needs a token and a chat id")
				}
				id, err = alert.StoreTelegram(ctx, q, d.Keys, accountID, in.Body.Label,
					auth.Secret(in.Body.Token), in.Body.ChatID)
			case alert.KindWebhook:
				if in.Body.URL == "" {
					return huma.Error400BadRequest("a webhook channel needs a url")
				}
				id, err = alert.StoreWebhook(ctx, q, d.Keys, accountID, in.Body.Label,
					auth.Secret(in.Body.URL))
			default:
				return huma.Error400BadRequest("unknown channel kind")
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		out := &struct {
			Body struct {
				ID uuid.UUID `json:"id"`
			}
		}{}
		out.Body.ID = id
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-alert-channel",
		Method:      http.MethodDelete,
		Path:        "/alert-channels/{id}",
		Summary:     "Stop sending alerts somewhere",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{}, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, huma.Error400BadRequest("malformed channel id")
		}
		var rows int64
		if err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			rows, err = q.DeleteAlertChannel(ctx, store.DeleteAlertChannelParams{
				AccountID: accountID, ID: id,
			})
			return err
		}); err != nil {
			return nil, err
		}
		if rows == 0 {
			return nil, huma.Error404NotFound("no such channel")
		}
		return &struct{}{}, nil
	})
}
