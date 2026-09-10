//go:build integration

package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// fakeVenue is a futures account that exists only in memory. The capture's own correctness
// is proven in internal/collateral; what this test is about is whether anything ever calls
// it, which is the half M2 shipped without for the projector.
type fakeVenue struct{ symbol string }

func (v fakeVenue) FuturesAccount(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{
		"totalMarginBalance":"1000","totalMaintMargin":"250","totalWalletBalance":"900",
		"totalUnrealizedProfit":"100","availableBalance":"250"}`), nil
}

func (v fakeVenue) PositionRisk(context.Context) (json.RawMessage, error) {
	return json.RawMessage(fmt.Sprintf(`[{"symbol":%q,"positionAmt":"0.5",
		"entryPrice":"60000","markPrice":"50000","liquidationPrice":"40000",
		"notional":"25000","leverage":"10","positionSide":"BOTH"}]`, v.symbol)), nil
}

func (v fakeVenue) LeverageBracket(context.Context) (json.RawMessage, error) {
	return json.RawMessage(fmt.Sprintf(`[{"symbol":%q,"brackets":[
		{"bracket":1,"notionalFloor":0,"notionalCap":50000,
		 "maintMarginRatio":0.01,"cum":0}]}]`, v.symbol)), nil
}

// fixedResolver answers with one instrument id, so the test does not depend on the alias
// table having been walked.
type fixedResolver struct{ id int64 }

func (r fixedResolver) Instrument(
	context.Context, instrument.Market, string, time.Time,
) (int64, error) {
	return r.id, nil
}

// capturedBuffer reads the stored snapshot with the account bound, because
// collateral_snapshots carries FORCE ROW LEVEL SECURITY and the owner is bound by it too --
// a bare owner query returns nothing and reads as "the capture never ran", which is the one
// answer this test must not get wrong.
//
// The pool is a parameter rather than opened here: this runs inside an eventually loop, and
// a pool per poll exhausts Postgres' connection slots long before the condition is met.
func capturedBuffer(pool *pgxpool.Pool, accountID, integrationID uuid.UUID) (string, bool) {
	ctx := context.Background()
	var buffer string
	err := tenancy.InTxRaw(ctx, pool, accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT (margin_balance - maintenance_margin)::text FROM collateral_snapshots
			  WHERE integration_id = $1`, integrationID).Scan(&buffer)
	})
	if err != nil {
		return "", false
	}
	return buffer, true
}

// The margin picture has to be driven by something, exactly as the fold does. A capture that
// nothing calls leaves /risk answering collateral_unavailable forever for an account whose
// exchange is answering perfectly -- and a capture loop shipped without the endpoint that
// reads it is the same mistake in the other direction, which is why both halves land here.
//
// Nothing in this test calls collateral.Capture. Running the supervisor is the whole of what
// it does.
func TestTheSupervisorCapturesTheMarginPictureOnItsTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedSpotInstrument(t)

	s, err := worker.NewSupervisor(worker.SupervisorConfig{
		DB:             appPool(t),
		AccountID:      accountID,
		IntegrationID:  integrationID,
		OwnerID:        "worker-collateral",
		LeaseTTL:       leaseTTL,
		HeartbeatEvery: 20 * time.Millisecond,
		ProjectEvery:   time.Hour,
		CaptureEvery:   10 * time.Millisecond,
		Stream:         newFakeStream(),
		Ingest:         &tradeIngester{accountID: accountID, integrationID: integrationID, instrumentID: instrumentID},
		Resync:         &fakeResyncer{},
		Backfill:       doneStepper{},
		Capture: worker.CollateralCapturer(worker.CaptureConfig{
			DB:            appPool(t),
			AccountID:     accountID,
			IntegrationID: integrationID,
			OwnerID:       "worker-collateral",
			Source:        fakeVenue{symbol: "BTCUSDT"},
			Resolver:      fixedResolver{id: instrumentID},
			Now:           time.Now,
		}),
		OnCaptureError: func(err error) { t.Log("capture: ", err) },
		Now:            func() time.Time { return supervisorNow },
	})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "the supervisor to go live", func() bool { return s.State() == ingest.StateLive })

	reader := ownerPool(t)
	eventually(t, "the margin picture to reach the database", func() bool {
		buffer, ok := capturedBuffer(reader, accountID, integrationID)
		return ok && buffer == "750.000000000000000000"
	})

	cancel()
	require.NoError(t, <-done)
}

// A capture that fails must not stop the ingestion. Live events are the one thing that
// cannot be recovered; a margin picture is asked for again in ten seconds. Killing the feed
// because the venue was briefly slow would trade a permanent loss for a temporary one --
// the same asymmetry the fold is given.
func TestAFailedCaptureDoesNotStopTheIngestion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedSpotInstrument(t)

	failures := make(chan error, 1)
	stream := newFakeStream()
	s, err := worker.NewSupervisor(worker.SupervisorConfig{
		DB:             appPool(t),
		AccountID:      accountID,
		IntegrationID:  integrationID,
		OwnerID:        "worker-capture-fails",
		LeaseTTL:       leaseTTL,
		HeartbeatEvery: 20 * time.Millisecond,
		CaptureEvery:   10 * time.Millisecond,
		Stream:         stream,
		Ingest:         &tradeIngester{accountID: accountID, integrationID: integrationID, instrumentID: instrumentID},
		Resync:         &fakeResyncer{},
		Backfill:       doneStepper{},
		Capture:        failingCapturer{},
		OnCaptureError: func(err error) {
			select {
			case failures <- err:
			default:
			}
		},
		Now: func() time.Time { return supervisorNow },
	})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "the supervisor to go live", func() bool { return s.State() == ingest.StateLive })

	select {
	case <-failures:
	case <-time.After(2 * time.Second):
		t.Fatal("the capture never failed, so this test proves nothing")
	}

	// And the feed is still being read.
	stream.messages <- frame("spot:trade:C:1")
	eventually(t, "the fill to reach the ledger despite the failed capture", func() bool {
		return eventCount(t, accountID, integrationID) == 1
	})

	cancel()
	require.NoError(t, <-done)
}

type failingCapturer struct{}

func (failingCapturer) Capture(context.Context) error {
	return fmt.Errorf("the venue said no")
}

// A capture is a write, so it carries the lease inside its own transaction like every other
// write this worker makes. Without it, a worker that has lost the integration keeps
// overwriting the margin picture the worker that now holds it is keeping current -- and the
// two would take turns, so the number on the screen would flicker between two accounts'
// worth of truth with nothing to say it was happening.
func TestACaptureByAWorkerWithoutTheLeaseIsRefusedByTheWriteItself(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedSpotInstrument(t)
	pool := appPool(t)

	held, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-holder", leaseTTL)
	require.NoError(t, err)
	require.True(t, held)

	capturer := worker.CollateralCapturer(worker.CaptureConfig{
		DB:            pool,
		AccountID:     accountID,
		IntegrationID: integrationID,
		OwnerID:       "worker-intruder",
		Source:        fakeVenue{symbol: "BTCUSDT"},
		Resolver:      fixedResolver{id: instrumentID},
		Now:           time.Now,
	})
	require.ErrorIs(t, capturer.Capture(ctx), worker.ErrLeaseLost)

	_, stored := capturedBuffer(ownerPool(t), accountID, integrationID)
	require.False(t, stored, "the intruder's capture was written anyway")
}

// fakeFuturesStream delivers frames on demand.
type fakeFuturesStream struct{ messages chan binance.Message }

func (f *fakeFuturesStream) Subscribe(context.Context) (<-chan binance.Message, error) {
	return f.messages, nil
}
func (f *fakeFuturesStream) Connected() bool { return true }
func (f *fakeFuturesStream) Close() error    { return nil }

// recordingResyncer remembers which symbols were replayed.
type recordingResyncer struct {
	mu      sync.Mutex
	symbols []string
}

func (r *recordingResyncer) ResyncSymbol(_ context.Context, symbol string, _, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.symbols = append(r.symbols, symbol)
	return nil
}

func (r *recordingResyncer) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.symbols...)
}

// A live USD-M fill triggers a bounded REST replay of that contract, and nothing else.
//
// The stream does not normalize the event. A fill's identity has to be the one the walk mints
// (L5); a second spelling would double the position. So the event says WHICH contract moved
// and the REST read says what happened -- a latency improvement that cannot invent a number.
func TestALiveFuturesFillTriggersAResyncOfThatSymbol(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedSpotInstrument(t)

	futures := &fakeFuturesStream{messages: make(chan binance.Message, 4)}
	resyncer := &recordingResyncer{}

	s, err := worker.NewSupervisor(worker.SupervisorConfig{
		DB:             appPool(t),
		AccountID:      accountID,
		IntegrationID:  integrationID,
		OwnerID:        "worker-futures",
		LeaseTTL:       leaseTTL,
		HeartbeatEvery: 20 * time.Millisecond,
		Stream:         newFakeStream(),
		Ingest:         &tradeIngester{accountID: accountID, integrationID: integrationID, instrumentID: instrumentID},
		Resync:         &fakeResyncer{},
		Backfill:       doneStepper{},
		FuturesStream:  futures,
		FuturesResync:  resyncer,
		Now:            func() time.Time { return supervisorNow },
	})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "the supervisor to go live", func() bool { return s.State() == ingest.StateLive })

	// A fill, an order acknowledgement, and an account update. Only the first is a fill.
	futures.messages <- binance.Message{Event: json.RawMessage(
		`{"e":"ORDER_TRADE_UPDATE","E":1789000000000,"o":{"s":"BTCUSDT","S":"BUY","x":"TRADE"}}`)}
	futures.messages <- binance.Message{Event: json.RawMessage(
		`{"e":"ACCOUNT_UPDATE","E":1789000000001,"a":{"B":[]}}`)}

	eventually(t, "the fill to trigger a replay", func() bool {
		return len(resyncer.seen()) == 1
	})
	require.Equal(t, []string{"BTCUSDT"}, resyncer.seen(),
		"the symbol replayed is not the one the event named")

	cancel()
	require.NoError(t, <-done)
}
