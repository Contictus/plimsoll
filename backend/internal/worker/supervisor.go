package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ingest"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
)

// maxResyncWindow is the widest window a gap replay may ask for. Binance rejects a
// myTrades range longer than 24 hours, so an unbounded resync is not a slow request -- it
// is a rejected one, and a resync that fails leaves its window silently unfilled (L11).
const maxResyncWindow = 24 * time.Hour

// defaultProjectEvery is how often the fold runs when nothing says otherwise. Two seconds
// is chosen from the reader's side, not the writer's: it is the longest a portfolio may
// silently lag a fill before the lag is worth more than the transactions saved.
const defaultProjectEvery = 2 * time.Second

// defaultFuturesResyncWindow covers a burst of fills in one request without approaching the
// venue's seven-day ceiling. Overlapping windows are free: identity deduplicates them (L5).
const defaultFuturesResyncWindow = time.Hour

// defaultCaptureEvery is how often the margin picture is refreshed when nothing says
// otherwise. Chosen against the API's collateral TTL rather than against the market: several
// captures may be missed before a reader is warned, so this is the interval at which "the
// loop is alive" stays true, not an attempt to track every tick of the mark.
const defaultCaptureEvery = 30 * time.Second

// ErrNotLeader means another worker holds the lease for this integration. It is a normal
// outcome on a fleet, not a failure: most workers lose most claims.
var ErrNotLeader = errors.New("worker: another worker holds this integration")

// StreamSource is the live feed, as the supervisor uses it. An interface rather than
// *binance.Stream so the supervisor's own behaviour -- gaps, disconnects, lease loss -- can
// be driven by hand instead of by a real exchange.
type StreamSource interface {
	Subscribe(ctx context.Context) (<-chan binance.Message, error)
	Connected() bool
	Close() error
}

// Ingester turns one raw stream event into canonical events. Returning none is normal and
// not an error: most execution reports are orders being placed or cancelled, which move no
// position.
type Ingester interface {
	Ingest(ctx context.Context, raw json.RawMessage) ([]ledger.Event, error)
}

// Resyncer replays one bounded window over REST. The supervisor never hands it a window
// wider than maxResyncWindow; splitting is the supervisor's job because the bound is a
// property of the venue, not of the caller.
type Resyncer interface {
	Resync(ctx context.Context, from, to time.Time) error
}

// Stepper does one chunk of historical work and reports whether more remains. A chunk, not
// the whole backfill: the supervisor has to be able to stop between chunks when the lease
// is lost, and a single call that ran for an hour could not.
type Stepper interface {
	Step(ctx context.Context) (more bool, err error)
}

// Projector folds the ledger this supervisor is filling into the position projection. An
// interface so a test can make the fold fail on demand; the default implementation is
// LedgerProjector, built from the supervisor's own fields.
type Projector interface {
	Project(ctx context.Context) error
}

// Capturer takes one picture of what the venue's margin engine currently believes. Separate
// from Stepper because it is not history being walked towards an end: it is a snapshot that
// is asked for again and again, and the previous answer is not built on.
type Capturer interface {
	Capture(ctx context.Context) error
}

// SymbolResyncer replays one symbol's fills over a bounded window. Separate from Resyncer,
// which replays every traded symbol after a gap: a live event names one contract, and
// resyncing the whole book for one fill would spend the account's weight budget on noise.
type SymbolResyncer interface {
	ResyncSymbol(ctx context.Context, symbol string, from, to time.Time) error
}

// SupervisorConfig is one integration's ingestion, assembled. Everything that touches time,
// the network or the database is injected, so the supervisor's own logic is what the tests
// exercise.
type SupervisorConfig struct {
	DB                       tenancy.Beginner
	AccountID, IntegrationID uuid.UUID

	// OwnerID identifies this worker process. Process-unique, minted per start -- never a
	// hostname, or two processes on one host would each believe they held the other's lease.
	OwnerID string

	LeaseTTL       time.Duration
	HeartbeatEvery time.Duration

	Stream   StreamSource
	Ingest   Ingester
	Resync   Resyncer
	Backfill Stepper

	// Project folds what has been ingested into positions. Optional: left nil it becomes a
	// LedgerProjector over this supervisor's own integration, which is what production
	// wants and what a test asserting the fold actually runs must not have to supply.
	Project Projector

	// ProjectEvery is how often the fold runs. A ticker rather than a call per event: the
	// fold is one transaction over every touched instrument, and running it per fill during
	// a busy minute would cost a transaction each for a number nobody read in between. The
	// cost of the interval is that a portfolio read can be that far behind the ledger, which
	// is reported rather than hidden (projection_lagging).
	ProjectEvery time.Duration

	// OnProjectError is called when a fold fails and the supervisor carries on anyway. It
	// is how an operator hears about it: this package holds no logger, and the alternative
	// to a callback is a failure nobody outside the process ever sees (L11).
	OnProjectError func(error)

	// FuturesStream is the USD-M user feed (F17). Optional: an account with no futures
	// wallet has nothing to listen to, and a supervisor that required one could not run a
	// spot-only account.
	//
	// It is a TRIGGER rather than an ingest path. The event names the contract that moved;
	// FuturesResync reads what actually happened over REST, because the identity of a fill
	// has to be the one the walk mints (L5) and a second spelling is a doubled position.
	FuturesStream StreamSource

	// FuturesResync replays the window a live event pointed at.
	FuturesResync SymbolResyncer

	// FuturesResyncWindow is how far back a triggered resync reads. Wide enough that a
	// burst of fills is covered by one request, narrow enough to stay inside the venue's
	// seven-day limit.
	FuturesResyncWindow time.Duration

	// Capture takes the margin picture on a ticker of its own. Optional and nil by default:
	// an integration with no futures wallet has nothing to capture, and a supervisor that
	// demanded one could not run a spot-only account at all.
	Capture Capturer

	// CaptureEvery is how often the margin picture is refreshed. Its cost is the reason it
	// is a ticker and not a per-event call: one capture is three signed requests, and the
	// buffer only changes when the mark does -- which is continuously, so there is no event
	// to hang it on.
	CaptureEvery time.Duration

	// OnCaptureError is called when a capture fails and the supervisor carries on anyway,
	// for the same reason OnProjectError exists: this package holds no logger, and the
	// alternative is a failure nobody outside the process ever sees (L11). The reader is
	// told independently, by /risk ageing into collateral_stale.
	OnCaptureError func(error)

	Now func() time.Time
}

// Supervisor runs the ingestion for one integration and says what state it is in.
//
// It is the only thing that writes for that integration, and it holds the lease that makes
// that true. Every write it makes is guarded by that lease inside the write's own
// transaction, so losing the lease stops it before anything further is committed rather
// than after the batch it happened to be on (L6).
type Supervisor struct {
	cfg SupervisorConfig

	mu         sync.Mutex
	conditions ingest.Conditions
	since      time.Time

	// changed carries "the state is different now" to the watchdog, which is the only
	// goroutine that writes. Buffered by one and never blocking: a change that arrives
	// while one is already pending is the same news, and losing it would be losing the
	// second of two identical messages.
	changed chan struct{}
}

// NewSupervisor validates the configuration. It claims nothing and connects nothing; Run
// does both.
func NewSupervisor(cfg SupervisorConfig) (*Supervisor, error) {
	switch {
	case cfg.DB == nil:
		return nil, errors.New("worker: supervisor needs a database")
	case cfg.AccountID == uuid.Nil || cfg.IntegrationID == uuid.Nil:
		return nil, errors.New("worker: supervisor needs an account and an integration")
	case cfg.OwnerID == "":
		return nil, ErrNoOwner
	case cfg.Stream == nil || cfg.Ingest == nil || cfg.Resync == nil || cfg.Backfill == nil:
		return nil, errors.New("worker: supervisor needs a stream, an ingester, a resyncer and a backfill")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.LeaseTTL < minTTL {
		cfg.LeaseTTL = time.Minute
	}
	if cfg.HeartbeatEvery <= 0 {
		// A third of the TTL: two heartbeats may be lost before the lease lapses, so a
		// single slow transaction does not hand the integration to another worker.
		cfg.HeartbeatEvery = cfg.LeaseTTL / 3
	}
	if cfg.FuturesResyncWindow <= 0 {
		cfg.FuturesResyncWindow = defaultFuturesResyncWindow
	}
	if cfg.CaptureEvery <= 0 {
		cfg.CaptureEvery = defaultCaptureEvery
	}
	if cfg.ProjectEvery <= 0 {
		cfg.ProjectEvery = defaultProjectEvery
	}
	if cfg.Project == nil {
		cfg.Project = LedgerProjector(cfg.DB, cfg.AccountID, cfg.IntegrationID, cfg.OwnerID)
	}
	return &Supervisor{cfg: cfg, since: cfg.Now(), changed: make(chan struct{}, 1)}, nil
}

// State is what this integration's ingestion is currently doing.
func (s *Supervisor) State() ingest.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ingest.Classify(s.conditions)
}

// Freshness is what the current state costs a reader, stamped with when it started. It is
// what a portfolio response embeds, and it is why the state enum exists at all (L11, K23).
func (s *Supervisor) Freshness() (freshness.Reason, bool) {
	s.mu.Lock()
	state, since := ingest.Classify(s.conditions), s.since
	s.mu.Unlock()

	reason, degraded := state.Reason()
	if !degraded {
		return freshness.Reason{}, false
	}
	reason.Since = since
	return reason, true
}

// Run claims the lease and ingests until the context is cancelled, the stream ends, or the
// lease is lost. It returns ErrNotLeader when another worker already holds the integration,
// and ErrLeaseLost when this one stops holding it.
func (s *Supervisor) Run(ctx context.Context) error {
	held, err := Claim(ctx, s.cfg.DB, s.cfg.AccountID, s.cfg.IntegrationID,
		s.cfg.OwnerID, s.cfg.LeaseTTL)
	if err != nil {
		return err
	}
	if !held {
		return ErrNotLeader
	}
	defer func() {
		// Best effort, and on the original context rather than the cancelled one: a clean
		// release saves the next worker a full TTL, and failing to release costs only that.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = Release(releaseCtx, s.cfg.DB, s.cfg.AccountID, s.cfg.IntegrationID, s.cfg.OwnerID)
	}()

	// Published before the subscribe, not after it. Subscribing can block or fail, and a
	// reader during that window must see "connecting" rather than whatever the last worker
	// left behind -- possibly "live", from a process that is gone.
	if err := s.publish(ctx); err != nil {
		return err
	}

	messages, err := s.cfg.Stream.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("worker: subscribe %s: %w", s.cfg.IntegrationID, err)
	}
	defer func() { _ = s.cfg.Stream.Close() }()

	s.update(func(c *ingest.Conditions) { c.Subscribed = true; c.Connected = s.cfg.Stream.Connected() })

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Three loops, deliberately. The backfill runs beside the stream rather than between
	// its events, because a chunk holding the only goroutine would stall the live feed for
	// as long as the chunk took -- and a stalled feed is a position that is wrong while
	// looking calm (K24). The watchdog is separate again so that a lease lost while both
	// are busy still stops them.
	var wg sync.WaitGroup
	failure := make(chan error, 6)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.runBackfill(runCtx); err != nil {
			failure <- err
			cancel()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.runProjector(runCtx); err != nil {
			failure <- err
			cancel()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.runFuturesStream(runCtx); err != nil {
			failure <- err
			cancel()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.runCapture(runCtx); err != nil {
			failure <- err
			cancel()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.runWatchdog(runCtx); err != nil {
			failure <- err
			cancel()
		}
	}()

	streamErr := s.runStream(runCtx, messages)
	cancel()
	wg.Wait()
	close(failure)

	// A lease lost anywhere is the answer, whichever loop noticed first: it is the only
	// outcome that says the writes stopped for a reason the caller must act on.
	for err := range failure {
		if errors.Is(err, ErrLeaseLost) {
			return err
		}
		if streamErr == nil {
			streamErr = err
		}
	}
	return streamErr
}

// runStream is the live path: one event at a time, appended under the lease.
func (s *Supervisor) runStream(ctx context.Context, messages <-chan binance.Message) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-messages:
			if !ok {
				return nil
			}
			if msg.Err != nil {
				if err := s.replay(ctx, msg.Err); err != nil {
					return err
				}
				continue
			}
			if err := s.ingest(ctx, msg.Event); err != nil {
				return err
			}
		}
	}
}

// ingest normalizes one event and appends it, with the lease checked inside the same
// transaction. A worker that lost its lease therefore commits nothing further -- not the
// event it was already holding, and not the one after it.
func (s *Supervisor) ingest(ctx context.Context, raw json.RawMessage) error {
	events, err := s.cfg.Ingest.Ingest(ctx, raw)
	if err != nil {
		return fmt.Errorf("worker: ingest on %s: %w", s.cfg.IntegrationID, err)
	}
	if len(events) == 0 {
		return nil
	}
	return tenancy.InTx(ctx, s.cfg.DB, s.cfg.AccountID, func(q *store.Queries) error {
		if err := GuardLease(ctx, q, s.cfg.AccountID, s.cfg.IntegrationID, s.cfg.OwnerID); err != nil {
			return err
		}
		_, err := ledger.Append(ctx, q, events)
		return err
	})
}

// replay fills the window the stream was disconnected for, in windows the venue will
// answer. Nothing in the protocol says what happened during that window, so treating it as
// empty is the silent loss the gap exists to prevent.
func (s *Supervisor) replay(ctx context.Context, gapErr error) error {
	var gap *binance.GapError
	if !errors.As(gapErr, &gap) {
		return fmt.Errorf("worker: stream on %s: %w", s.cfg.IntegrationID, gapErr)
	}

	s.update(func(c *ingest.Conditions) { c.Resyncing = true; c.Connected = s.cfg.Stream.Connected() })
	defer s.update(func(c *ingest.Conditions) { c.Resyncing = false })

	for from := gap.From; from.Before(gap.To); {
		to := from.Add(maxResyncWindow)
		if to.After(gap.To) {
			to = gap.To
		}
		if err := s.cfg.Resync.Resync(ctx, from, to); err != nil {
			return fmt.Errorf("worker: resync %s..%s on %s: %w",
				from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339),
				s.cfg.IntegrationID, err)
		}
		from = to
	}
	return nil
}

// runBackfill works through history one chunk at a time, beside the live feed.
func (s *Supervisor) runBackfill(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		more, err := s.cfg.Backfill.Step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("worker: backfill on %s: %w", s.cfg.IntegrationID, err)
		}
		if !more {
			s.update(func(c *ingest.Conditions) { c.HistoryComplete = true })
			return nil
		}
	}
}

// runWatchdog holds the lease and watches the connection. Both on one ticker because both
// are questions about whether this worker may still speak for the integration, and asking
// them together keeps the answer consistent.
func (s *Supervisor) runWatchdog(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.HeartbeatEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.changed:
			// A state change is news, and news waits for no ticker. Publishing here rather
			// than in update() keeps every write for this integration on one goroutine,
			// which is what makes "one writer" a property of the code and not of the
			// schedule.
			if err := s.publish(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		case <-ticker.C:
			// The connection first: a stream that stops delivering has to be reported at
			// once. Its gap only arrives when the connection is back, so a supervisor that
			// waited for it would show a live feed for the whole outage.
			s.update(func(c *ingest.Conditions) { c.Connected = s.cfg.Stream.Connected() })

			alive, err := Heartbeat(ctx, s.cfg.DB, s.cfg.AccountID, s.cfg.IntegrationID,
				s.cfg.OwnerID, s.cfg.LeaseTTL)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("worker: heartbeat on %s: %w", s.cfg.IntegrationID, err)
			}
			if !alive {
				return fmt.Errorf("%w: %s stopped holding %s",
					ErrLeaseLost, s.cfg.OwnerID, s.cfg.IntegrationID)
			}

			// Republished on every tick even when nothing changed. The state is not the
			// only thing the row carries: updated_at is how a reader tells a worker that is
			// live now from one that was live when it died.
			if err := s.publish(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// update applies a change to the conditions and stamps when the state last changed, so
// freshness can say how long it has been that way rather than only what is wrong.
func (s *Supervisor) update(change func(*ingest.Conditions)) {
	s.mu.Lock()
	before := ingest.Classify(s.conditions)
	change(&s.conditions)
	moved := ingest.Classify(s.conditions) != before
	if moved {
		s.since = s.cfg.Now()
	}
	s.mu.Unlock()

	if !moved {
		return
	}
	// Non-blocking on purpose. update runs on the stream and backfill goroutines, and a
	// state change must never wait on the watchdog to be ready -- a supervisor that stalled
	// its own ingestion to report on it would be reporting on nothing.
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// runProjector folds the ledger into the positions on a ticker, beside the ingestion that
// is filling it.
//
// A failed fold does not stop the supervisor, and that asymmetry is the point. Live events
// are the one thing that cannot be recovered: a stream not being read is data gone, while a
// projection is by definition rebuildable from the ledger (L3). Killing ingestion because a
// projection failed would trade a permanent loss for a temporary one.
//
// This is not the same as hiding it. A reader is told about a projection that has fallen
// behind by the API comparing the cursor to the ledger, which is exactly the symptom of a
// projector that is failing -- so the reader learns of it whether or not the worker is
// alive to report it (L11). OnProjectError exists so an operator hears it too.
func (s *Supervisor) runProjector(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.ProjectEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.cfg.Project.Project(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// A lost lease is the exception: it does not mean the fold failed, it
				// means this worker is no longer entitled to run one, and everything else
				// it is doing has to stop for the same reason.
				if errors.Is(err, ErrLeaseLost) {
					return err
				}
				if s.cfg.OnProjectError != nil {
					s.cfg.OnProjectError(err)
				}
			}
		}
	}
}

// runFuturesStream turns live USD-M events into bounded REST resyncs.
//
// The stream is a latency improvement: without it a fill reaches the ledger at the next
// backfill chunk, and with it within a second. What it deliberately does NOT do is normalize
// the event itself -- the fill's identity has to match what the walk mints (L5), and this
// build has not verified the payload's field names against the venue's own page. A stream
// that invented an identity would double every futures position it touched.
//
// A failed resync does not stop the supervisor, for the same reason a failed fold does not:
// the walk will read the same window again, and the live feed is the half that cannot be
// recovered.
func (s *Supervisor) runFuturesStream(ctx context.Context) error {
	if s.cfg.FuturesStream == nil || s.cfg.FuturesResync == nil {
		return nil
	}
	messages, err := s.cfg.FuturesStream.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("worker: subscribe futures %s: %w", s.cfg.IntegrationID, err)
	}
	defer func() { _ = s.cfg.FuturesStream.Close() }()

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, open := <-messages:
			if !open {
				return nil
			}
			if msg.Err != nil {
				// A gap on this stream is not a gap in the ledger: the walk covers three
				// months and the resync below covers the last hour, so the window is read
				// either way. Nothing to replay, nothing to report.
				continue
			}
			symbol, ok := binance.FuturesEventSymbol(msg.Event)
			if !ok {
				// Most frames are orders being placed and cancelled. Ignoring them is
				// normal; treating them as errors would stop a healthy supervisor.
				continue
			}
			to := s.cfg.Now().UTC()
			from := to.Add(-s.cfg.FuturesResyncWindow)
			if err := s.cfg.FuturesResync.ResyncSymbol(ctx, symbol, from, to); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if errors.Is(err, ErrLeaseLost) {
					return err
				}
				if s.cfg.OnProjectError != nil {
					s.cfg.OnProjectError(err)
				}
			}
		}
	}
}

// runCapture refreshes the margin picture on a ticker, beside the ingestion.
//
// A failed capture does not stop the supervisor, for the same reason a failed fold does not:
// live events are the one thing that cannot be recovered, and a margin picture is asked for
// again in one interval. The reader is told anyway -- /risk ages the stored snapshot into
// collateral_stale on its own, so a capture loop that has quietly died is visible to the
// person looking at the number whether or not this process is alive to say so (L11).
func (s *Supervisor) runCapture(ctx context.Context) error {
	if s.cfg.Capture == nil {
		return nil
	}
	ticker := time.NewTicker(s.cfg.CaptureEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.cfg.Capture.Capture(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// A lost lease is the exception, as everywhere else: it does not mean the
				// venue failed, it means this worker is no longer entitled to write.
				if errors.Is(err, ErrLeaseLost) {
					return err
				}
				if s.cfg.OnCaptureError != nil {
					s.cfg.OnCaptureError(err)
				}
			}
		}
	}
}

// publish writes what this supervisor is doing where the API can read it, with the lease
// checked in the same transaction. A worker that lost its lease stops describing an
// integration it no longer writes -- the alternative is a dead worker's "live" outliving it
// in the row a portfolio response is built from (K39).
func (s *Supervisor) publish(ctx context.Context) error {
	s.mu.Lock()
	state, since := ingest.Classify(s.conditions), s.since
	s.mu.Unlock()

	return tenancy.InTx(ctx, s.cfg.DB, s.cfg.AccountID, func(q *store.Queries) error {
		if err := GuardLease(ctx, q, s.cfg.AccountID, s.cfg.IntegrationID, s.cfg.OwnerID); err != nil {
			return err
		}
		return ingest.Publish(ctx, q, s.cfg.AccountID, s.cfg.IntegrationID,
			s.cfg.OwnerID, state, since)
	})
}
