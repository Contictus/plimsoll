package alert

import (
	"context"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// RunDeps is one account's evaluation pass, assembled.
//
// Channels are supplied already built, and already decrypted, by whoever holds the key
// provider. This package never sees a stored credential: it is handed things that can send,
// which keeps the decryption path in one place and keeps a bot token out of a package whose
// job is arithmetic.
type RunDeps struct {
	DB            tenancy.Beginner
	AccountID     uuid.UUID
	Window        portfolio.Window
	CollateralTTL time.Duration
	Channels      []Deliverer
}

// Run evaluates every rule this account has against the numbers as they now stand, records
// what it found, and delivers what is worth saying. It returns how many rules changed state.
//
// It evaluates only when a valuation run has completed (ARCHITECTURE section 7). A per-tick
// evaluation fires on prices that never entered a published total -- a message about a number
// the user was never shown and cannot check against any screen this system serves.
func Run(ctx context.Context, d RunDeps) (int, error) {
	exposure, err := portfolio.LoadExposure(ctx, d.DB, d.AccountID, d.Window, d.CollateralTTL)
	if err != nil {
		return 0, err
	}
	if exposure.Run == nil {
		return 0, nil
	}

	rules, state, err := loadRules(ctx, d)
	if err != nil {
		return 0, err
	}
	if len(rules) == 0 {
		return 0, nil
	}

	values := MetricsOf(exposure)
	firings, next := Evaluate(rules, values, state, d.Window.Now)
	if len(firings) == 0 {
		return 0, nil
	}

	// Recorded first, delivered second, and the record is written whether or not the delivery
	// works: an account whose channel expired still has a history of what happened while
	// nobody was being told.
	ids, err := record(ctx, d, firings, next, exposure.Run.RunID)
	if err != nil {
		return 0, err
	}
	deliver(ctx, d, firings, ids)
	return len(firings), nil
}

// loadRules reads the rules and the state the last pass left behind.
func loadRules(ctx context.Context, d RunDeps) ([]Rule, map[uuid.UUID]State, error) {
	var (
		rules []Rule
		state = map[uuid.UUID]State{}
	)
	err := tenancy.InTx(ctx, d.DB, d.AccountID, func(q *store.Queries) error {
		rows, err := q.ListAlertRules(ctx, d.AccountID)
		if err != nil {
			return fmt.Errorf("alert: read rules for %s: %w", d.AccountID, err)
		}
		for _, r := range rows {
			rules = append(rules, Rule{
				ID:         r.ID,
				Metric:     r.Metric,
				Scope:      Scope{Kind: r.ScopeKind, Name: r.ScopeName},
				Comparator: Comparator(r.Comparator),
				Trigger:    r.TriggerAt,
				Clear:      r.ClearAt,
				Cooldown:   time.Duration(r.CooldownSeconds) * time.Second,
				Enabled:    r.Enabled,
			})
			if r.Firing == nil {
				continue
			}
			s := State{RuleID: r.ID, Firing: *r.Firing}
			if r.Since != nil {
				s.Since = *r.Since
			}
			if r.LastFiredAt != nil {
				s.LastFiredAt = *r.LastFiredAt
			}
			state[r.ID] = s
		}
		return nil
	})
	return rules, state, err
}

// record writes the alerts and the new state in ONE transaction. A state that advanced
// without its alert row would be a condition the system believes it has announced and never
// did -- silence that looks exactly like safety.
func record(
	ctx context.Context, d RunDeps, firings []Firing, next map[uuid.UUID]State, runID int64,
) (map[uuid.UUID]uuid.UUID, error) {
	ids := map[uuid.UUID]uuid.UUID{}
	err := tenancy.InTx(ctx, d.DB, d.AccountID, func(q *store.Queries) error {
		for _, f := range firings {
			var value decimal.NullDecimal
			if f.Value.Valid {
				value = f.Value
			}
			id, err := q.InsertAlert(ctx, store.InsertAlertParams{
				AccountID: d.AccountID,
				RuleID:    f.RuleID,
				Kind:      string(f.Kind),
				Metric:    f.Metric,
				ScopeKind: f.Scope.Kind,
				ScopeName: f.Scope.Name,
				Value:     value,
				RunID:     &runID,
				FiredAt:   f.At,
			})
			if err != nil {
				return fmt.Errorf("alert: record firing for rule %s: %w", f.RuleID, err)
			}
			ids[f.RuleID] = id
		}

		for id, s := range next {
			var since, lastFired *time.Time
			if !s.Since.IsZero() {
				at := s.Since
				since = &at
			}
			if !s.LastFiredAt.IsZero() {
				at := s.LastFiredAt
				lastFired = &at
			}
			if err := q.UpsertAlertState(ctx, store.UpsertAlertStateParams{
				RuleID: id, AccountID: d.AccountID, Firing: s.Firing,
				Since: since, LastFiredAt: lastFired,
			}); err != nil {
				return fmt.Errorf("alert: save state for rule %s: %w", id, err)
			}
		}
		return nil
	})
	return ids, err
}

// deliver sends what is worth sending and records what happened. A delivery failure is not
// returned: the alert is already recorded, and failing the whole pass over a channel that is
// down would mean the next pass re-evaluates from the state it never saved.
func deliver(ctx context.Context, d RunDeps, firings []Firing, ids map[uuid.UUID]uuid.UUID) {
	for _, f := range firings {
		if f.Kind == Unavailable {
			// Recorded, not sent. A gap is not a breach, and messaging the user every time
			// a metric is briefly missing is the fastest way to teach them to ignore the
			// channel.
			continue
		}
		m := Message{Title: title(f), Body: body(f), Kind: f.Kind, At: f.At}

		var failure error
		for _, channel := range d.Channels {
			if err := channel.Deliver(ctx, m); err != nil {
				failure = err
			}
		}
		markDelivery(ctx, d, ids[f.RuleID], failure)
	}
}

func markDelivery(ctx context.Context, d RunDeps, alertID uuid.UUID, failure error) {
	if alertID == uuid.Nil {
		return
	}
	_ = tenancy.InTx(ctx, d.DB, d.AccountID, func(q *store.Queries) error {
		if failure == nil {
			return q.MarkAlertDelivered(ctx, store.MarkAlertDeliveredParams{
				AccountID: d.AccountID, ID: alertID, DeliveredAt: timePtr(d.Window.Now),
			})
		}
		// The channels redact their own errors before returning them (L13), which is what
		// makes writing one here safe: this column is read by a human debugging a channel,
		// and a Telegram URL would carry the token into it.
		text := failure.Error()
		return q.MarkAlertUndelivered(ctx, store.MarkAlertUndeliveredParams{
			AccountID: d.AccountID, ID: alertID, DeliveryError: &text,
		})
	})
}

func timePtr(t time.Time) *time.Time { return &t }

func title(f Firing) string {
	scope := f.Scope.Kind
	if f.Scope.Name != "" {
		scope = f.Scope.Name
	}
	if f.Kind == Resolved {
		return fmt.Sprintf("%s %s recovered", scope, f.Metric)
	}
	return fmt.Sprintf("%s %s", scope, f.Metric)
}

func body(f Firing) string {
	value := "unknown"
	if f.Value.Valid {
		// A string, all the way to the message: a leverage rendered through a float is
		// rounded in the digits the threshold was set in (L1).
		value = f.Value.Decimal.String()
	}
	return fmt.Sprintf("%s %s is %s (%s)", f.Scope.Kind, f.Metric, value, f.Kind)
}
