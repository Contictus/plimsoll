//go:build integration

package alert_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/alert"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/projection"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// recorder is a channel that remembers instead of sending.
type recorder struct {
	messages []alert.Message
	fail     error
}

func (r *recorder) Deliver(_ context.Context, m alert.Message) error {
	r.messages = append(r.messages, m)
	return r.fail
}

func window() portfolio.Window {
	return portfolio.Window{Now: time.Now().UTC(), LeaseTTL: time.Minute, PriceTTL: time.Hour}
}

// seedLeveragedAccount builds an account whose portfolio the valuation can price: one funded
// integration, one bought position, one price, one run.
func seedLeveragedAccount(t *testing.T) (accountID uuid.UUID, quoteAssetID int64) {
	t.Helper()
	ctx := context.Background()
	accountID = seedAccount(t)
	integrationID := uuid.New()

	var baseID int64
	owner := ownerPool(t)
	require.NoError(t, owner.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"AB-"+uuid.NewString()).Scan(&baseID))
	require.NoError(t, owner.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'stablecoin') RETURNING id`,
		"AQ-"+uuid.NewString()).Scan(&quoteAssetID))
	var instrumentID int64
	require.NoError(t, owner.QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id)
		 VALUES ($1, 'spot', $2, $3) RETURNING id`,
		"AI-"+uuid.NewString(), baseID, quoteAssetID).Scan(&instrumentID))

	require.NoError(t, tenancy.InTxRaw(ctx, owner, accountID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'alerting')`, integrationID, accountID)
		return err
	}))

	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	events := []ledger.Event{{
		AccountID: accountID, IntegrationID: integrationID,
		VenueEventID: "spot:deposit:1", VenueSequence: 1, Source: "rest",
		EventType: ledger.TypeDeposit, AssetID: &quoteAssetID,
		Quantity:  decimal.NewNullDecimal(decimal.RequireFromString("1000")),
		EventTime: at, Raw: json.RawMessage(`{}`),
	}, {
		AccountID: accountID, IntegrationID: integrationID,
		VenueEventID: "spot:trade:1", VenueSequence: 2, Source: "rest",
		EventType: ledger.TypeTrade, InstrumentID: &instrumentID, Side: ledger.SideBuy,
		Quantity:  decimal.NewNullDecimal(decimal.RequireFromString("4")),
		Price:     decimal.NewNullDecimal(decimal.RequireFromString("100")),
		EventTime: at.Add(time.Second), Raw: json.RawMessage(`{}`),
	}}
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		_, err := ledger.Append(ctx, q, events)
		return err
	}))
	_, err := projection.Project(ctx, appPool(t), accountID, integrationID)
	require.NoError(t, err)

	now := time.Now().UTC()
	_, err = owner.Exec(ctx,
		`INSERT INTO price_ticks (instrument_id, ts, price, source, observed_at)
		 VALUES ($1, $2, $3, 'test', $4)`,
		instrumentID, now.Add(-time.Minute).Truncate(time.Minute),
		decimal.RequireFromString("200"), now.Add(-time.Minute))
	require.NoError(t, err)
	return accountID, quoteAssetID
}

func produceRunFor(t *testing.T, pegAssetID int64) {
	t.Helper()
	_, err := valuation.Produce(context.Background(), store.New(ownerPool(t)),
		"test:alerting", time.Now().UTC(),
		valuation.PegSet{pegAssetID: decimal.RequireFromString("1")})
	require.NoError(t, err)
}

func addRule(
	t *testing.T, accountID uuid.UUID, metric string, comparator alert.Comparator,
	trigger, clear string,
) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		var err error
		id, err = q.CreateAlertRule(ctx, store.CreateAlertRuleParams{
			AccountID: accountID, Name: "rule-" + uuid.NewString(), Metric: metric,
			ScopeKind: alert.ScopePortfolio, ScopeName: "", Comparator: string(comparator),
			TriggerAt: decimal.RequireFromString(trigger),
			ClearAt:   decimal.RequireFromString(clear),
			Enabled:   true,
		})
		return err
	}))
	return id
}

func storedAlerts(t *testing.T, accountID uuid.UUID) []store.ListAlertsRow {
	t.Helper()
	ctx := context.Background()
	var rows []store.ListAlertsRow
	require.NoError(t, tenancy.InTx(ctx, appPool(t), accountID, func(q *store.Queries) error {
		var err error
		rows, err = q.ListAlerts(ctx, store.ListAlertsParams{AccountID: accountID, MaxRows: 50})
		return err
	}))
	return rows
}

func runOnce(t *testing.T, accountID uuid.UUID, channels ...alert.Deliverer) int {
	t.Helper()
	fired, err := alert.Run(context.Background(), alert.RunDeps{
		DB: appPool(t), AccountID: accountID, Window: window(),
		CollateralTTL: time.Hour, Channels: channels,
	})
	require.NoError(t, err)
	return fired
}

func init() { _ = fmt.Sprint }

// ARCHITECTURE section 7: alerts evaluate on COMPLETED valuation runs, not per tick.
//
// Per-tick evaluation fires on prices that never entered a published total -- a message about
// a number the user was never shown and cannot check against any screen this system serves.
//
// The instant is 2019 rather than now, because valuation runs are registry-wide rather than
// per account: in a database other tests have used there is always SOME run, and asking as of
// an instant before any of them is the only way to put this account in the state a fresh
// install is in. Nothing else about the account differs.
func TestNoValuationRunMeansNoAlert(t *testing.T) {
	accountID, _ := seedLeveragedAccount(t)
	addRule(t, accountID, alert.MetricGrossExposure, alert.Above, "1", "0")

	channel := &recorder{}
	fired, err := alert.Run(context.Background(), alert.RunDeps{
		DB: appPool(t), AccountID: accountID, CollateralTTL: time.Hour,
		Window: portfolio.Window{
			Now:      time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
			LeaseTTL: time.Minute, PriceTTL: time.Hour,
		},
		Channels: []alert.Deliverer{channel},
	})
	require.NoError(t, err)
	require.Zero(t, fired)
	require.Empty(t, channel.messages)
	require.Empty(t, storedAlerts(t, accountID),
		"an alert was recorded from prices that never entered a published total")
}

// And an account the current run cannot price does not fire either: the metric is absent, so
// the rule reports a gap rather than deciding the account is safe (L11).
func TestARunThatCannotPriceThisAccountFiresNothing(t *testing.T) {
	accountID, _ := seedLeveragedAccount(t)
	addRule(t, accountID, alert.MetricGrossExposure, alert.Above, "1", "0")

	channel := &recorder{}
	runOnce(t, accountID, channel)

	require.Empty(t, channel.messages, "an alert was sent about a number nobody could compute")
	for _, a := range storedAlerts(t, accountID) {
		require.Equal(t, "unavailable", a.Kind)
	}
}

// And with a run behind it, the rule fires, is recorded, and is delivered.
func TestARuleFiresOnACompletedRunAndIsDeliveredAndRecorded(t *testing.T) {
	accountID, pegID := seedLeveragedAccount(t)
	addRule(t, accountID, alert.MetricGrossExposure, alert.Above, "500", "400")
	produceRunFor(t, pegID)

	channel := &recorder{}
	require.Equal(t, 1, runOnce(t, accountID, channel))

	require.Len(t, channel.messages, 1)
	require.Contains(t, channel.messages[0].Body, "gross_exposure")

	stored := storedAlerts(t, accountID)
	require.Len(t, stored, 1)
	require.Equal(t, "fired", stored[0].Kind)
	require.NotNil(t, stored[0].DeliveredAt, "a delivered alert was not marked delivered")
	require.NotNil(t, stored[0].RunID,
		"the alert does not name the run it was decided from, so it cannot be traced to prices")
}

// The second pass says nothing. This is the flapping property at the level that matters: the
// state survives in the database, so a worker that restarts does not re-announce every
// condition that was already announced.
func TestASecondPassOverAnUnchangedConditionSaysNothing(t *testing.T) {
	accountID, pegID := seedLeveragedAccount(t)
	addRule(t, accountID, alert.MetricGrossExposure, alert.Above, "500", "400")
	produceRunFor(t, pegID)

	channel := &recorder{}
	require.Equal(t, 1, runOnce(t, accountID, channel))
	require.Zero(t, runOnce(t, accountID, channel),
		"the same condition was announced twice; the state did not survive the first pass")
	require.Len(t, channel.messages, 1)
	require.Len(t, storedAlerts(t, accountID), 1)
}

// A failing channel does not lose the alert. The row is the record and the channel is the
// transport, so an account whose Telegram token expired still has a history of what happened
// while nobody was being told.
func TestAFailedDeliveryStillRecordsTheAlert(t *testing.T) {
	accountID, pegID := seedLeveragedAccount(t)
	addRule(t, accountID, alert.MetricGrossExposure, alert.Above, "500", "400")
	produceRunFor(t, pegID)

	channel := &recorder{fail: fmt.Errorf("alert: the channel refused the message with status 500")}
	require.Equal(t, 1, runOnce(t, accountID, channel))

	stored := storedAlerts(t, accountID)
	require.Len(t, stored, 1)
	require.Nil(t, stored[0].DeliveredAt)
	require.NotNil(t, stored[0].DeliveryError)
	require.Contains(t, *stored[0].DeliveryError, "500")
}

// A rule watching a metric nothing computed reports `unavailable` and does not fire. Unknown
// is not "below the threshold": a margin buffer nobody could compute is not a safe one (L11).
func TestARuleOnAnUncomputedMetricRecordsUnavailable(t *testing.T) {
	accountID, pegID := seedLeveragedAccount(t)
	addRule(t, accountID, alert.MetricMarginBuffer, alert.Below, "1000", "1500")
	produceRunFor(t, pegID)

	channel := &recorder{}
	runOnce(t, accountID, channel)

	stored := storedAlerts(t, accountID)
	require.Len(t, stored, 1)
	require.Equal(t, "unavailable", stored[0].Kind)
	require.False(t, stored[0].Value.Valid, "an absent metric was recorded as a number")
	require.Empty(t, channel.messages,
		"an unavailable metric was delivered as an alert; it is a gap, not a breach")
}
