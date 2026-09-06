// Package ratelimit spends the exchange's request budget on our behalf. It exists because
// Binance counts weight per IP, not per key (K24): one server serving many accounts spends
// one budget, so a per-key limiter alone leaves every key healthy while the IP earns a ban
// of up to three days.
//
// Every outbound call passes Acquire. Time is injected, so the whole thing is testable
// without waiting out a minute.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

var (
	// ErrNoCeiling means the caller did not supply a budget. Deliberately not defaulted:
	// the ceiling is read from exchangeInfo's rateLimits at connect time, and a hardcoded
	// number is one that will be wrong after Binance changes it -- we would find out from
	// the ban.
	ErrNoCeiling = errors.New("ratelimit: a per-minute ceiling is required, read it from exchangeInfo")

	// ErrWeightExceedsBudget means no amount of waiting can satisfy this call. Returning it
	// immediately matters: blocking forever would look exactly like a slow exchange, and
	// would be diagnosed as one.
	ErrWeightExceedsBudget = errors.New("ratelimit: weight is larger than the whole budget")

	// ErrInvalidWeight means a caller asked for zero or negative weight. Every Binance
	// endpoint costs at least one, so this is a bug in the call site rather than a state
	// worth handling.
	ErrInvalidWeight = errors.New("ratelimit: weight must be positive")
)

// Priority orders callers competing for the same budget. A user is watching the realtime
// request; the backfill has hours to finish.
type Priority int

const (
	// PriorityBackfill is history that will complete eventually. It yields to everything.
	PriorityBackfill Priority = iota
	// PriorityReconcile is a scheduled correctness check: it should not be starved by
	// backfill, and it should not delay a live stream.
	PriorityReconcile
	// PriorityRealtime is a request someone is waiting on, including the bounded resync
	// that follows a websocket gap.
	PriorityRealtime
)

// Clock is the time this package reads. Injected rather than taken from the runtime (L4)
// so a test can cross a minute boundary without spending one.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has elapsed. A buffered channel, so a
	// caller that has already given up does not block the clock.
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the production Clock.
type SystemClock struct{}

// Now reports the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// After returns a channel that receives once d has elapsed.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config is the budget, in weight per minute. Both ceilings come from the exchange:
// SharedPerMinute is the IP's REQUEST_WEIGHT limit from exchangeInfo, and
// PerIntegrationPerMinute is how much of it any one integration may spend, which is our
// policy rather than the exchange's.
type Config struct {
	SharedPerMinute         int
	PerIntegrationPerMinute int
}

// Limiter enforces both tiers. A call is admitted only when the shared budget and the
// integration's own budget can both pay for it, and only when no penalty is in force.
type Limiter struct {
	cfg   Config
	clock Clock

	mu             sync.Mutex
	shared         *rate.Limiter
	perIntegration map[uuid.UUID]*rate.Limiter
	penaltyUntil   time.Time

	// queue holds every caller currently waiting, in arrival order. Only the highest
	// priority waiter may take tokens, which is what makes a later realtime call overtake
	// an earlier backfill one. A slice with a linear scan rather than a heap: the queue is
	// a handful of in-flight requests, and this version can be read and believed.
	queue   []*waiter
	nextSeq uint64

	// wake is closed and replaced whenever the state changes, so every waiter re-evaluates
	// instead of holding a stale view of who is at the head.
	wake chan struct{}
}

type waiter struct {
	priority Priority
	seq      uint64
}

// New builds a Limiter. It refuses a missing ceiling rather than defaulting one.
func New(cfg Config, clock Clock) (*Limiter, error) {
	if cfg.SharedPerMinute <= 0 || cfg.PerIntegrationPerMinute <= 0 {
		return nil, fmt.Errorf("%w: shared=%d per-integration=%d",
			ErrNoCeiling, cfg.SharedPerMinute, cfg.PerIntegrationPerMinute)
	}
	return &Limiter{
		cfg:            cfg,
		clock:          clock,
		shared:         newBucket(cfg.SharedPerMinute),
		perIntegration: make(map[uuid.UUID]*rate.Limiter),
		wake:           make(chan struct{}),
	}, nil
}

// newBucket models a per-minute ceiling as a token bucket: the burst is the whole minute's
// budget, refilling steadily. Binance enforces a rolling window, which a bucket
// approximates from the safe side -- it never allows more in any minute than the ceiling.
func newBucket(perMinute int) *rate.Limiter {
	return rate.NewLimiter(rate.Limit(float64(perMinute)/60.0), perMinute)
}

// Acquire blocks until weight can be spent on behalf of integrationID, or until ctx is
// done. It charges nothing unless it returns nil: a caller that gives up or is cancelled
// leaves the budget exactly as it found it.
//
// weight rather than a call count, because that is what varies between endpoints: myTrades
// at 20 and a status check at 1 must not look like two calls.
func (l *Limiter) Acquire(
	ctx context.Context, integrationID uuid.UUID, priority Priority, weight int,
) error {
	if weight <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidWeight, weight)
	}
	// Checked before enqueueing: a caller that is already cancelled is not going to make
	// the request, so it must not spend budget even when the budget is full.
	if err := ctx.Err(); err != nil {
		return err
	}

	w := l.enqueue(priority)
	defer l.dequeue(w)

	for {
		l.mu.Lock()
		if !l.isHead(w) {
			// Someone outranks us. Nothing to compute; wait to be told the state changed.
			wake := l.wake
			l.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wake:
			}
			continue
		}

		now := l.clock.Now()
		var delay time.Duration
		switch {
		case now.Before(l.penaltyUntil):
			delay = l.penaltyUntil.Sub(now)
		default:
			ok, wait, err := l.charge(now, integrationID, weight)
			if err != nil {
				l.mu.Unlock()
				return err
			}
			if ok {
				l.mu.Unlock()
				return nil
			}
			delay = wait
		}

		// The timer is created while the lock is held. Reading the clock and arming the
		// timer must be one step, or a clock that moves in between produces a wait
		// measured from the wrong instant.
		timer := l.clock.After(delay)
		wake := l.wake
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer:
		case <-wake:
		}
	}
}

// Penalize stops every integration for retryAfter. The 429 was earned by the IP, so the
// penalty lands on the IP: scoping it to the integration that tripped it would leave the
// others hammering an endpoint that has already started counting toward a ban.
//
// The longest penalty wins. A shorter one arriving second must not shorten the wait.
func (l *Limiter) Penalize(retryAfter time.Duration) {
	if retryAfter <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	until := l.clock.Now().Add(retryAfter)
	if until.After(l.penaltyUntil) {
		l.penaltyUntil = until
	}
	l.notify()
}

// PenalizedUntil reports when the current penalty lifts, zero if none is in force. It is
// what the supervisor turns into a freshness reason rather than staying silent (L11).
func (l *Limiter) PenalizedUntil() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.clock.Now().Before(l.penaltyUntil) {
		return l.penaltyUntil
	}
	return time.Time{}
}

// Waiting is how many callers are queued. Useful as a signal that backfill is starving
// behind realtime, and it is the synchronisation point the tests rely on.
func (l *Limiter) Waiting() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue)
}

// charge admits the call if both tiers can pay for it right now. It must be all or
// nothing: taking from the shared budget and then finding the integration's own budget
// short would spend weight on a call that never happens.
//
// Caller holds l.mu.
func (l *Limiter) charge(now time.Time, integrationID uuid.UUID, weight int) (
	bool, time.Duration, error,
) {
	own := l.bucketFor(integrationID)

	shared := l.shared.ReserveN(now, weight)
	mine := own.ReserveN(now, weight)
	if !shared.OK() || !mine.OK() {
		// A reservation that is not OK never took tokens; cancelling is a no-op but keeps
		// the two paths symmetrical.
		shared.CancelAt(now)
		mine.CancelAt(now)
		return false, 0, fmt.Errorf("%w: weight %d, shared budget %d, integration budget %d",
			ErrWeightExceedsBudget, weight, l.cfg.SharedPerMinute, l.cfg.PerIntegrationPerMinute)
	}

	sharedDelay, myDelay := shared.DelayFrom(now), mine.DelayFrom(now)
	if sharedDelay == 0 && myDelay == 0 {
		return true, 0, nil
	}

	// Cancelled at the same instant they were made, so every token is returned.
	shared.CancelAt(now)
	mine.CancelAt(now)

	wait := sharedDelay
	if myDelay > wait {
		wait = myDelay
	}
	return false, wait, nil
}

// bucketFor lazily creates an integration's budget. Caller holds l.mu.
func (l *Limiter) bucketFor(integrationID uuid.UUID) *rate.Limiter {
	bucket, ok := l.perIntegration[integrationID]
	if !ok {
		bucket = newBucket(l.cfg.PerIntegrationPerMinute)
		l.perIntegration[integrationID] = bucket
	}
	return bucket
}

func (l *Limiter) enqueue(priority Priority) *waiter {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.nextSeq++
	w := &waiter{priority: priority, seq: l.nextSeq}
	l.queue = append(l.queue, w)
	// A new arrival can displace the current head, so everyone re-evaluates.
	l.notify()
	return w
}

func (l *Limiter) dequeue(w *waiter) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for i, other := range l.queue {
		if other == w {
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			break
		}
	}
	l.notify()
}

// isHead reports whether w outranks every other waiter: highest priority first, and within
// one priority the order of arrival, so a backfill cannot be starved by later backfills.
//
// Caller holds l.mu.
func (l *Limiter) isHead(w *waiter) bool {
	for _, other := range l.queue {
		if other == w {
			continue
		}
		if other.priority > w.priority {
			return false
		}
		if other.priority == w.priority && other.seq < w.seq {
			return false
		}
	}
	return true
}

// notify wakes every waiter. Closing and replacing the channel is a broadcast that needs
// no bookkeeping per waiter. Caller holds l.mu.
func (l *Limiter) notify() {
	close(l.wake)
	l.wake = make(chan struct{})
}
