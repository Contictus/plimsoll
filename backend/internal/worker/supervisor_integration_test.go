//go:build integration

package worker_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

var supervisorNow = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

// fakeStream is a live feed a test drives by hand.
type fakeStream struct {
	messages  chan binance.Message
	connected bool
	mu        sync.Mutex
	closed    bool
}

func newFakeStream() *fakeStream {
	return &fakeStream{messages: make(chan binance.Message, 8), connected: true}
}

func (f *fakeStream) Subscribe(context.Context) (<-chan binance.Message, error) {
	return f.messages, nil
}

func (f *fakeStream) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

// Close deliberately does not close the channel. The real stream owns its channel and is
// the only sender; here the test is the sender, so closing it on shutdown would make every
// test that pushes an event while the supervisor is stopping a panic rather than an
// assertion. The buffer absorbs a send nobody reads, which is exactly the outcome a test
// asserting "nothing further is written" wants.
func (f *fakeStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// fakeIngester turns any frame into one storable event, keyed by whatever the frame says.
// Normalization is tested in the binance package; what matters here is that the supervisor
// writes what it is given, under its lease, and not otherwise.
type fakeIngester struct {
	accountID, integrationID uuid.UUID
	assetID                  int64
}

func (f fakeIngester) Ingest(_ context.Context, raw json.RawMessage) ([]ledger.Event, error) {
	var frame struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return nil, err
	}
	return []ledger.Event{{
		AccountID:     f.accountID,
		IntegrationID: f.integrationID,
		VenueEventID:  frame.ID,
		Source:        binance.SourceStream,
		EventType:     ledger.TypeDeposit,
		AssetID:       &f.assetID,
		Quantity:      decimal.NewNullDecimal(decimal.RequireFromString("1")),
		EventTime:     supervisorNow,
		Raw:           raw,
	}}, nil
}

// fakeResyncer records the windows it was asked to replay.
type fakeResyncer struct {
	mu      sync.Mutex
	windows [][2]time.Time
}

func (f *fakeResyncer) Resync(_ context.Context, from, to time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.windows = append(f.windows, [2]time.Time{from.UTC(), to.UTC()})
	return nil
}

func (f *fakeResyncer) recorded() [][2]time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]time.Time(nil), f.windows...)
}

// blockingStepper holds one backfill chunk open until a test releases it.
type blockingStepper struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingStepper() *blockingStepper {
	return &blockingStepper{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingStepper) Step(ctx context.Context) (bool, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// doneStepper has no history left to load.
type doneStepper struct{}

func (doneStepper) Step(context.Context) (bool, error) { return false, nil }

func frame(id string) binance.Message {
	return binance.Message{Event: json.RawMessage(`{"id":"` + id + `"}`)}
}

func newSupervisor(
	t *testing.T, accountID, integrationID uuid.UUID, owner string,
	stream worker.StreamSource, resync worker.Resyncer, stepper worker.Stepper,
) *worker.Supervisor {
	t.Helper()
	return newSupervisorBeating(t, accountID, integrationID, owner, stream, resync, stepper,
		20*time.Millisecond)
}

// newSupervisorBeating exposes the heartbeat interval, so a test can take the watchdog out
// of the picture and leave exactly one mechanism to do the work it is asserting.
func newSupervisorBeating(
	t *testing.T, accountID, integrationID uuid.UUID, owner string,
	stream worker.StreamSource, resync worker.Resyncer, stepper worker.Stepper,
	heartbeat time.Duration,
) *worker.Supervisor {
	t.Helper()
	s, err := worker.NewSupervisor(worker.SupervisorConfig{
		DB:             appPool(t),
		AccountID:      accountID,
		IntegrationID:  integrationID,
		OwnerID:        owner,
		LeaseTTL:       leaseTTL,
		HeartbeatEvery: heartbeat,
		Stream:         stream,
		Ingest:         fakeIngester{accountID, integrationID, seedAsset(t)},
		Resync:         resync,
		Backfill:       stepper,
		Now:            func() time.Time { return supervisorNow },
	})
	require.NoError(t, err)
	return s
}

func eventCount(t *testing.T, accountID, integrationID uuid.UUID) int {
	t.Helper()
	ctx := context.Background()
	var n int
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_events WHERE integration_id = $1`,
			integrationID).Scan(&n)
	}))
	return n
}

// eventually polls for a condition rather than sleeping a guessed interval. A sleep long
// enough to be reliable is long enough to be slow, and one short enough to be fast is a
// flake.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Two supervisors on one integration is the race the lease exists to prevent, so the second
// must decline to run at all rather than start and discover it later.
func TestASupervisorWithoutTheLeaseDoesNotRun(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	held, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-a", leaseTTL)
	require.NoError(t, err)
	require.True(t, held)

	stream := newFakeStream()
	t.Cleanup(func() { _ = stream.Close() })
	s := newSupervisor(t, accountID, integrationID, "worker-b", stream, &fakeResyncer{}, doneStepper{})

	require.ErrorIs(t, s.Run(ctx), worker.ErrNotLeader)
	require.Equal(t, ingest.StateConnecting, s.State(),
		"a supervisor that never ran must not claim to be live")
}

// A gap is replayed in bounded windows. Binance caps a myTrades time range at 24 hours, so
// an unbounded resync is not a slow request -- it is a rejected one, and a resync that fails
// leaves the window silently unfilled (L11).
func TestAGapIsReplayedInBoundedWindowsAndThenLiveAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)

	stream := newFakeStream()
	resync := &fakeResyncer{}
	s := newSupervisor(t, accountID, integrationID, "worker-a", stream, resync, doneStepper{})

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "the supervisor to go live", func() bool { return s.State() == ingest.StateLive })

	from := supervisorNow.Add(-50 * time.Hour)
	stream.messages <- binance.Message{Err: &binance.GapError{From: from, To: supervisorNow}}

	eventually(t, "the gap to be replayed", func() bool { return len(resync.recorded()) > 0 })
	eventually(t, "the supervisor to be live again", func() bool {
		return s.State() == ingest.StateLive
	})

	windows := resync.recorded()
	require.Len(t, windows, 3, "50 hours is two full 24-hour windows and a short one")
	require.Equal(t, from, windows[0][0], "the replay must start where the gap did")
	for i, w := range windows {
		require.LessOrEqual(t, w[1].Sub(w[0]), 24*time.Hour,
			"window %d exceeds what the venue will answer", i)
		if i > 0 {
			require.Equal(t, windows[i-1][1], w[0], "a gap between windows is a gap in the ledger")
		}
	}
	require.Equal(t, supervisorNow, windows[len(windows)-1][1], "the replay must reach the present")

	// Events flow again afterwards.
	stream.messages <- frame("spot:deposit:after-gap")
	eventually(t, "the event after the gap", func() bool {
		return eventCount(t, accountID, integrationID) == 1
	})

	cancel()
	require.NoError(t, <-done)
}

// K24: history yields to realtime. A backfill chunk holding the supervisor's only goroutine
// would stall the live feed for as long as the chunk took, and a stalled feed is a position
// that is wrong while looking calm.
func TestABackfillChunkDoesNotBlockRealtime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)

	stream := newFakeStream()
	stepper := newBlockingStepper()
	s := newSupervisor(t, accountID, integrationID, "worker-a", stream, &fakeResyncer{}, stepper)

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	<-stepper.entered
	require.Equal(t, ingest.StateBackfilling, s.State(),
		"history is still loading, and a reader is entitled to know")

	stream.messages <- frame("spot:deposit:during-backfill")
	eventually(t, "a live event while the backfill chunk is still running", func() bool {
		return eventCount(t, accountID, integrationID) == 1
	})

	close(stepper.release)
	eventually(t, "the supervisor to go live once history is loaded", func() bool {
		return s.State() == ingest.StateLive
	})

	cancel()
	require.NoError(t, <-done)
}

// THE TEST THE LEASE EXISTS FOR, from the writing end -- and it is two tests, because there
// are two mechanisms and one test cannot tell which of them did the work.
//
// This one takes the watchdog out of the picture with an hour-long heartbeat, so the only
// thing that can stop the write is the guard inside the write's own transaction. Without
// it the event would commit while another worker was already the writer.
func TestAnEventArrivingAfterTheLeaseIsLostIsRefusedByTheWriteItself(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)

	stream := newFakeStream()
	s := newSupervisorBeating(t, accountID, integrationID, "worker-a", stream,
		&fakeResyncer{}, doneStepper{}, time.Hour)

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "the supervisor to go live", func() bool { return s.State() == ingest.StateLive })

	stream.messages <- frame("spot:deposit:while-leader")
	eventually(t, "the first event", func() bool {
		return eventCount(t, accountID, integrationID) == 1
	})

	expireLease(t, accountID, integrationID)
	taken, err := worker.Claim(context.Background(), appPool(t),
		accountID, integrationID, "worker-b", leaseTTL)
	require.NoError(t, err)
	require.True(t, taken)

	stream.messages <- frame("spot:deposit:after-losing-it")

	select {
	case err := <-done:
		require.ErrorIs(t, err, worker.ErrLeaseLost)
	case <-time.After(10 * time.Second):
		t.Fatal("the write went through without the lease")
	}
	require.Equal(t, 1, eventCount(t, accountID, integrationID),
		"nothing may be written after the lease is gone")
}

// The other half: a quiet account. No event arrives to be refused, so the only thing that
// can notice the lease is gone is the heartbeat. A supervisor that kept holding a stream it
// no longer owns would come back to life the moment the account traded.
func TestTheWatchdogStopsASupervisorThatLostItsLeaseWhileIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)

	stream := newFakeStream()
	s := newSupervisor(t, accountID, integrationID, "worker-a", stream, &fakeResyncer{}, doneStepper{})

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "the supervisor to go live", func() bool { return s.State() == ingest.StateLive })

	expireLease(t, accountID, integrationID)

	select {
	case err := <-done:
		require.ErrorIs(t, err, worker.ErrLeaseLost)
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor kept running without its lease")
	}
	require.Zero(t, eventCount(t, accountID, integrationID))
}

// A stream that stops delivering is reported at once. The gap that follows only arrives
// when the connection is back, so a supervisor that waited for it would show a live feed
// for as long as the outage lasted -- which is the confident-and-wrong failure L11 rejects.
func TestADisconnectedStreamIsReportedWithoutWaitingForItsGap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)

	stream := newFakeStream()
	s := newSupervisor(t, accountID, integrationID, "worker-a", stream, &fakeResyncer{}, doneStepper{})

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "the supervisor to go live", func() bool { return s.State() == ingest.StateLive })

	stream.mu.Lock()
	stream.connected = false
	stream.mu.Unlock()

	eventually(t, "the disconnect to be reported", func() bool {
		return s.State() == ingest.StateDegraded
	})
	reason, degraded := s.State().Reason()
	require.True(t, degraded)
	require.Equal(t, "ws_gap", reason.Code)

	cancel()
	require.NoError(t, <-done)
}

// seedSpotInstrument creates one tradeable pair as the owner. Reference data has no RLS
// and no write grant for the app role (00004, 00005), so this is the only way in.
func seedSpotInstrument(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)

	var base, quote, id int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'native') RETURNING id`,
		"WB-"+uuid.NewString()).Scan(&base))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO assets (canonical_symbol, kind) VALUES ($1, 'stablecoin') RETURNING id`,
		"WQ-"+uuid.NewString()).Scan(&quote))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO instruments (canonical_symbol, kind, base_asset_id, quote_asset_id)
		 VALUES ($1, 'spot', $2, $3) RETURNING id`,
		"W-"+uuid.NewString(), base, quote).Scan(&id))
	return id
}

// tradeIngester turns a frame into a buy that moves a position, so a test can assert on the
// projection rather than only on the ledger.
type tradeIngester struct {
	accountID, integrationID uuid.UUID
	instrumentID             int64
	seq                      int64
	mu                       sync.Mutex
}

func (f *tradeIngester) Ingest(_ context.Context, raw json.RawMessage) ([]ledger.Event, error) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.seq++
	seq := f.seq
	f.mu.Unlock()

	return []ledger.Event{{
		AccountID:     f.accountID,
		IntegrationID: f.integrationID,
		VenueEventID:  body.ID,
		VenueSequence: seq,
		Source:        binance.SourceStream,
		EventType:     ledger.TypeTrade,
		InstrumentID:  &f.instrumentID,
		Side:          ledger.SideBuy,
		Quantity:      decimal.NewNullDecimal(decimal.RequireFromString("2")),
		Price:         decimal.NewNullDecimal(decimal.RequireFromString("100")),
		EventTime:     supervisorNow.Add(time.Duration(seq) * time.Second),
		Raw:           raw,
	}}, nil
}

// positionQuantity reads the projection with the account bound, because positions carries
// FORCE ROW LEVEL SECURITY and the owner is bound by it too -- a bare owner query returns
// nothing and reads as "the fold did not run", which is the one answer this test must not
// get wrong.
//
// It takes the pool rather than opening one: it is called from inside an eventually loop,
// and a pool per poll exhausts Postgres' connection slots in seconds.
func positionQuantity(
	pool *pgxpool.Pool, accountID, integrationID uuid.UUID, instrumentID int64,
) (string, bool) {
	ctx := context.Background()
	var qty string
	err := tenancy.InTxRaw(ctx, pool, accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT quantity::text FROM positions
			 WHERE integration_id = $1 AND instrument_id = $2`,
			integrationID, instrumentID).Scan(&qty)
	})
	if err != nil {
		return "", false
	}
	return qty, true
}

// The fold has to be driven by something. M2 shipped a projector nothing called, so the
// ledger filled and `positions` stayed empty -- an endpoint reading it would have answered
// "you hold nothing" for an account with a full history, which is the confident-and-wrong
// failure L11 exists to reject.
//
// Nothing in this test calls projection.Project. Ingesting is the whole of what it does.
func TestTheSupervisorFoldsWhatItIngestsIntoThePositionProjection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)
	instrumentID := seedSpotInstrument(t)

	stream := newFakeStream()
	s, err := worker.NewSupervisor(worker.SupervisorConfig{
		DB:             appPool(t),
		AccountID:      accountID,
		IntegrationID:  integrationID,
		OwnerID:        "worker-a",
		LeaseTTL:       leaseTTL,
		HeartbeatEvery: 20 * time.Millisecond,
		ProjectEvery:   10 * time.Millisecond,
		Stream:         stream,
		Ingest:         &tradeIngester{accountID: accountID, integrationID: integrationID, instrumentID: instrumentID},
		Resync:         &fakeResyncer{},
		Backfill:       doneStepper{},
		OnProjectError: func(err error) { t.Log("projection: ", err) },
		Now:            func() time.Time { return supervisorNow },
	})
	require.NoError(t, err)

	reader := ownerPool(t)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "the supervisor to go live", func() bool { return s.State() == ingest.StateLive })

	stream.messages <- frame("spot:trade:W:1")
	eventually(t, "the fill to reach the ledger", func() bool {
		return eventCount(t, accountID, integrationID) == 1
	})
	eventually(t, "the first fill to reach the projection", func() bool {
		qty, ok := positionQuantity(reader, accountID, integrationID, instrumentID)
		return ok && qty == "2.000000000000000000"
	})

	// A second fill must fold onto the first, not replace it: the projector resumes from
	// its cursor, and a run that reset would leave the same quantity behind and look right.
	stream.messages <- frame("spot:trade:W:2")
	eventually(t, "the second fill to fold onto the first", func() bool {
		qty, ok := positionQuantity(reader, accountID, integrationID, instrumentID)
		return ok && qty == "4.000000000000000000"
	})

	cancel()
	require.NoError(t, <-done)
}

// The API runs in a different process from the worker, often on a different machine, so it
// cannot ask the supervisor what state it is in. The state has to be written down, and it
// has to be written down promptly -- a portfolio that says "live" for forty seconds after
// the feed died is the failure this whole enum was built to prevent (K39).
func TestTheSupervisorPublishesItsStateWhereTheAPICanReadIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)

	stream := newFakeStream()
	// A heartbeat long enough that nothing in this test can be explained by the ticker:
	// what is asserted is that a state *change* publishes at once.
	s := newSupervisorBeating(t, accountID, integrationID, "worker-a", stream,
		&fakeResyncer{}, doneStepper{}, time.Hour)

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	reader := appPool(t)
	readState := func() (ingest.Status, bool) {
		statuses, err := ingest.ReadStatus(context.Background(), reader, accountID)
		require.NoError(t, err)
		for _, st := range statuses {
			if st.IntegrationID == integrationID {
				return st, true
			}
		}
		return ingest.Status{}, false
	}

	eventually(t, "the live state to be published", func() bool {
		st, ok := readState()
		return ok && st.State == ingest.StateLive
	})

	stream.mu.Lock()
	stream.connected = false
	stream.mu.Unlock()
	stream.messages <- binance.Message{
		Err: &binance.GapError{From: supervisorNow.Add(-time.Minute), To: supervisorNow},
	}

	eventually(t, "the degraded state to be published", func() bool {
		st, ok := readState()
		return ok && st.State == ingest.StateDegraded
	})

	cancel()
	require.NoError(t, <-done)
}

// publishAs writes a status row the way a worker does, so a test can drive the upsert
// directly rather than through a supervisor's schedule.
func publishAs(
	t *testing.T, pool *pgxpool.Pool, accountID, integrationID uuid.UUID,
	owner string, state ingest.State, since time.Time,
) {
	t.Helper()
	require.NoError(t, tenancy.InTx(context.Background(), pool, accountID,
		func(q *store.Queries) error {
			return ingest.Publish(context.Background(), q, accountID, integrationID,
				owner, state, since)
		}))
}

func statusOf(
	t *testing.T, pool *pgxpool.Pool, accountID, integrationID uuid.UUID,
) ingest.Status {
	t.Helper()
	statuses, err := ingest.ReadStatus(context.Background(), pool, accountID)
	require.NoError(t, err)
	for _, s := range statuses {
		if s.IntegrationID == integrationID {
			return s
		}
	}
	t.Fatalf("no status for integration %s", integrationID)
	return ingest.Status{}
}

// since answers "how long has it been like this", and a heartbeat republishing the same
// state every forty seconds must not keep answering "a few seconds". A feed that has been
// down for an hour would then report as freshly down every time anyone looked -- which is
// worse than not reporting the duration at all, because it reads as a blip.
func TestRepublishingTheSameStateDoesNotResetHowLongItHasBeenThatWay(t *testing.T) {
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	began := supervisorNow
	publishAs(t, pool, accountID, integrationID, "worker-a", ingest.StateDegraded, began)
	require.Equal(t, began.UTC(), statusOf(t, pool, accountID, integrationID).Since.UTC())

	// The same state again, an hour later, as a heartbeat would.
	publishAs(t, pool, accountID, integrationID, "worker-a", ingest.StateDegraded,
		began.Add(time.Hour))
	require.Equal(t, began.UTC(), statusOf(t, pool, accountID, integrationID).Since.UTC(),
		"a heartbeat republishing the same state must not restart the clock on it")

	// A different state does move it: that is what since means.
	recovered := began.Add(2 * time.Hour)
	publishAs(t, pool, accountID, integrationID, "worker-a", ingest.StateLive, recovered)
	got := statusOf(t, pool, accountID, integrationID)
	require.Equal(t, ingest.StateLive, got.State)
	require.Equal(t, recovered.UTC(), got.Since.UTC())
}

// A worker that no longer holds the lease must stop describing the integration, not only
// stop writing to it. The published state is what a portfolio response is built from, so a
// dead worker's "live" outliving it is the confident-and-wrong failure in its purest form
// (K37, K39).
//
// The heartbeat is set to an hour so the watchdog cannot be what stops it: the only
// mechanism left is the lease check inside the publish's own transaction.
func TestAStateChangeAfterTheLeaseIsLostIsRefusedByThePublishItself(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	stream := newFakeStream()
	s := newSupervisorBeating(t, accountID, integrationID, "worker-a", stream,
		&fakeResyncer{}, doneStepper{}, time.Hour)

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	eventually(t, "the live state to be published", func() bool {
		return statusOf(t, pool, accountID, integrationID).State == ingest.StateLive
	})

	// The lease goes away underneath the running supervisor.
	require.NoError(t, worker.Release(ctx, pool, accountID, integrationID, "worker-a"))

	// A state change it is no longer entitled to report.
	stream.mu.Lock()
	stream.connected = false
	stream.mu.Unlock()
	stream.messages <- binance.Message{
		Err: &binance.GapError{From: supervisorNow.Add(-time.Minute), To: supervisorNow},
	}

	require.ErrorIs(t, <-done, worker.ErrLeaseLost)
	require.Equal(t, ingest.StateLive, statusOf(t, pool, accountID, integrationID).State,
		"the supervisor wrote a state it no longer held the lease for")
}
