package portfolio_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func active(id uuid.UUID) ingest.Status {
	return ingest.Status{
		IntegrationID: id, Exchange: "binance", Label: "main", Configured: "active",
		Reported: true, State: ingest.StateLive,
	}
}

func snapshotAt(id uuid.UUID, asOf time.Time, margin, maintenance string) portfolio.CollateralSnapshot {
	return portfolio.CollateralSnapshot{
		IntegrationID:     id,
		AsOf:              asOf,
		CapturedAt:        asOf,
		MarginBalance:     decimal.RequireFromString(margin),
		WalletBalance:     decimal.RequireFromString(margin),
		MaintenanceMargin: decimal.RequireFromString(maintenance),
	}
}

// The buffer is margin balance less the maintenance requirement, and it comes from the one
// snapshot the response names in as_of (L10).
func TestRiskReportsTheBufferFromTheSnapshotItNames(t *testing.T) {
	id := uuid.New()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	asOf := now.Add(-30 * time.Second)

	got := portfolio.BuildRisk(portfolio.RiskInput{
		Now:           now,
		CollateralTTL: 5 * time.Minute,
		Statuses:      []ingest.Status{active(id)},
		Snapshots:     []portfolio.CollateralSnapshot{snapshotAt(id, asOf, "1000", "250")},
		Positions: []portfolio.CollateralPosition{{
			IntegrationID:    id,
			InstrumentID:     7,
			Symbol:           "BTC-USDT-PERP",
			Quantity:         dec("0.5"),
			EntryPrice:       dec("60000"),
			MarkPrice:        dec("50000"),
			LiquidationPrice: dec("40000"),
			Notional:         dec("25000"),
			Leverage:         dec("10"),
			MaintMargin:      dec("250"),
		}},
	})

	require.Equal(t, asOf, got.AsOf, "as_of must name the snapshot the numbers came from")
	require.Equal(t, freshness.StatusOK, got.Freshness.Status)
	require.Len(t, got.Integrations, 1)

	one := got.Integrations[0]
	require.Equal(t, "750", one.MarginBuffer.String())
	require.Len(t, one.Positions, 1)
	// |50000 - 40000| / 50000
	require.True(t, one.Positions[0].Distance.Valid)
	require.Equal(t, "0.2", one.Positions[0].Distance.Decimal.String())
}

// L11: a snapshot older than the tolerance is still served, and it says so.
func TestAStaleSnapshotIsServedWithItsReason(t *testing.T) {
	id := uuid.New()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	asOf := now.Add(-2 * time.Hour)

	got := portfolio.BuildRisk(portfolio.RiskInput{
		Now:           now,
		CollateralTTL: 5 * time.Minute,
		Statuses:      []ingest.Status{active(id)},
		Snapshots:     []portfolio.CollateralSnapshot{snapshotAt(id, asOf, "1000", "250")},
	})

	require.Equal(t, freshness.StatusDegraded, got.Freshness.Status)
	require.Contains(t, codesOf(got.Freshness), freshness.ReasonCollateralStale)
	require.Len(t, got.Integrations, 1, "a stale snapshot is served, not withheld")
	require.Equal(t, "750", got.Integrations[0].MarginBuffer.String())

	for _, r := range got.Freshness.Reasons {
		if r.Code == freshness.ReasonCollateralStale {
			require.Equal(t, asOf, r.Since, "since must be when the number stopped being current")
		}
	}
}

// A zero margin buffer and an unknown one are opposite claims. An account with no capture
// must render as no integration at all plus an error, never as a buffer of zero.
func TestAMissingSnapshotIsNeverAZeroBuffer(t *testing.T) {
	id := uuid.New()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	got := portfolio.BuildRisk(portfolio.RiskInput{
		Now:           now,
		CollateralTTL: 5 * time.Minute,
		Statuses:      []ingest.Status{active(id)},
	})

	require.Equal(t, freshness.StatusUnreliable, got.Freshness.Status)
	require.Contains(t, codesOf(got.Freshness), freshness.ReasonCollateralUnavailable)
	require.Empty(t, got.Integrations,
		"an integration with no capture must not appear with zeroed numbers")
}

// A paused integration has no worker by the operator's own decision, so a missing capture
// is expected rather than a fault. Reporting it as an error would teach a reader to ignore
// the code on the day it means something.
func TestAPausedIntegrationDoesNotRaiseCollateralUnavailable(t *testing.T) {
	id := uuid.New()
	paused := active(id)
	paused.Configured = "paused"

	got := portfolio.BuildRisk(portfolio.RiskInput{
		Now:           time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		CollateralTTL: 5 * time.Minute,
		Statuses:      []ingest.Status{paused},
	})

	require.NotContains(t, codesOf(got.Freshness), freshness.ReasonCollateralUnavailable)
}

// as_of is the oldest snapshot in the response, not the newest. A response is only as
// current as its stalest part, and naming the newest would date the whole screen by its
// luckiest number.
func TestAsOfIsTheOldestSnapshotInTheResponse(t *testing.T) {
	old, fresh := uuid.New(), uuid.New()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	older := now.Add(-4 * time.Minute)

	got := portfolio.BuildRisk(portfolio.RiskInput{
		Now:           now,
		CollateralTTL: 5 * time.Minute,
		Statuses:      []ingest.Status{active(old), active(fresh)},
		Snapshots: []portfolio.CollateralSnapshot{
			snapshotAt(fresh, now.Add(-10*time.Second), "1000", "250"),
			snapshotAt(old, older, "2000", "500"),
		},
	})

	require.Equal(t, older, got.AsOf)
}
