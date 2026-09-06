package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/httpapi"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
)

// maxResyncWindow is the widest window a gap replay may ask for. Binance rejects a
// myTrades range longer than 24 hours, so an unbounded resync is not a slow request -- it
// is a rejected one, and a resync that fails leaves its window silently unfilled (L11).
const maxResyncWindow = 24 * time.Hour

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
	conditions Conditions
	since      time.Time
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
	return &Supervisor{cfg: cfg, since: cfg.Now()}, nil
}

// State is what this integration's ingestion is currently doing.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Classify(s.conditions)
}

// Freshness is what the current state costs a reader, stamped with when it started. It is
// what a portfolio response embeds, and it is why the state enum exists at all (L11, K23).
func (s *Supervisor) Freshness() (httpapi.Reason, bool) {
	s.mu.Lock()
	state, since := Classify(s.conditions), s.since
	s.mu.Unlock()

	reason, degraded := state.Reason()
	if !degraded {
		return httpapi.Reason{}, false
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

	messages, err := s.cfg.Stream.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("worker: subscribe %s: %w", s.cfg.IntegrationID, err)
	}
	defer func() { _ = s.cfg.Stream.Close() }()

	s.update(func(c *Conditions) { c.Subscribed = true; c.Connected = s.cfg.Stream.Connected() })

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Three loops, deliberately. The backfill runs beside the stream rather than between
	// its events, because a chunk holding the only goroutine would stall the live feed for
	// as long as the chunk took -- and a stalled feed is a position that is wrong while
	// looking calm (K24). The watchdog is separate again so that a lease lost while both
	// are busy still stops them.
	var wg sync.WaitGroup
	failure := make(chan error, 3)

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

	s.update(func(c *Conditions) { c.Resyncing = true; c.Connected = s.cfg.Stream.Connected() })
	defer s.update(func(c *Conditions) { c.Resyncing = false })

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
			s.update(func(c *Conditions) { c.HistoryComplete = true })
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
		case <-ticker.C:
			// The connection first: a stream that stops delivering has to be reported at
			// once. Its gap only arrives when the connection is back, so a supervisor that
			// waited for it would show a live feed for the whole outage.
			s.update(func(c *Conditions) { c.Connected = s.cfg.Stream.Connected() })

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
		}
	}
}

// update applies a change to the conditions and stamps when the state last changed, so
// freshness can say how long it has been that way rather than only what is wrong.
func (s *Supervisor) update(change func(*Conditions)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := Classify(s.conditions)
	change(&s.conditions)
	if Classify(s.conditions) != before {
		s.since = s.cfg.Now()
	}
}
