package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A ceiling of 100 per minute means one unit of weight every 600ms, which is the only
// arithmetic these tests depend on.
const perMinute = 100

var tokenInterval = time.Minute / perMinute

// advanceOneToken moves the clock just past the refill of a single unit of weight. The
// extra millisecond is not slack in the assertion: rate.Limit is a float, so the exact
// refill instant carries a few nanoseconds of dust, and landing on it is a coin flip. A
// millisecond is far below the 600ms it takes to earn the next token, so no test can pass
// by accident because of it.
func advanceOneToken(clock *fakeClock) {
	clock.Advance(tokenInterval + time.Millisecond)
}

func newTestLimiter(t *testing.T, shared, perIntegration int) (*Limiter, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	l, err := New(Config{
		SharedPerMinute:         shared,
		PerIntegrationPerMinute: perIntegration,
	}, clock)
	require.NoError(t, err)
	return l, clock
}

// waitUntilWaiting blocks until n callers are queued. It is a synchronisation point, not a
// timeout-based guess: once a caller is queued it cannot be served until the test advances
// the clock, so what follows is deterministic.
func waitUntilWaiting(t *testing.T, l *Limiter, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for l.Waiting() != n {
		if time.Now().After(deadline) {
			t.Fatalf("expected %d waiting, got %d", n, l.Waiting())
		}
		time.Sleep(time.Millisecond)
	}
}

// requireStillWaiting proves a caller stays blocked. It watches for a window rather than
// polling once: the correct limiter leaves the caller blocked until the clock moves again,
// so any window is enough -- but a single poll right after Advance would race a broken
// limiter that is midway through serving the call, and report it as still waiting.
func requireStillWaiting(t *testing.T, errCh <-chan error) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			t.Fatalf("expected the caller to still be waiting, it returned %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
}

// requireServed turns "never served" into a named failure instead of a suite-wide timeout,
// so a run that goes wrong says which caller starved.
func requireServed(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the caller was never served")
	}
}

// The test the shared tier exists for. Binance's limits are per IP, not per key (K24), so
// one server serving many accounts spends one budget. A per-key limiter alone leaves every
// key healthy while the IP earns a ban of up to three days.
func TestTheSharedBudgetBlocksAnIntegrationThatIsWithinItsOwn(t *testing.T) {
	ctx := context.Background()
	l, clock := newTestLimiter(t, perMinute, perMinute)
	a, b := uuid.New(), uuid.New()

	// A drains the shared budget. Its own budget is drained too, but B's is untouched.
	require.NoError(t, l.Acquire(ctx, a, PriorityBackfill, perMinute))

	errCh := make(chan error, 1)
	go func() { errCh <- l.Acquire(ctx, b, PriorityBackfill, 1) }()
	waitUntilWaiting(t, l, 1)
	requireStillWaiting(t, errCh)

	advanceOneToken(clock)
	requireServed(t, errCh)
}

// An integration must still be stopped by its own budget while the shared one has room:
// otherwise one backfill can spend the whole server's minute.
func TestAnIntegrationIsAlsoBoundByItsOwnBudget(t *testing.T) {
	ctx := context.Background()
	l, clock := newTestLimiter(t, 10*perMinute, perMinute)
	a := uuid.New()

	require.NoError(t, l.Acquire(ctx, a, PriorityBackfill, perMinute))

	errCh := make(chan error, 1)
	go func() { errCh <- l.Acquire(ctx, a, PriorityBackfill, 1) }()
	waitUntilWaiting(t, l, 1)
	requireStillWaiting(t, errCh)

	advanceOneToken(clock)
	requireServed(t, errCh)
}

// A weight no budget can ever satisfy must fail immediately. Blocking forever on it would
// look exactly like a slow exchange, and would be diagnosed as one.
func TestAWeightLargerThanTheBudgetFailsFast(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLimiter(t, perMinute, perMinute)

	err := l.Acquire(ctx, uuid.New(), PriorityRealtime, perMinute+1)
	require.ErrorIs(t, err, ErrWeightExceedsBudget)
	require.Zero(t, l.Waiting())

	// Same when only the per-integration budget is too small: a caller must not be told
	// its request is fine because the shared tier could have taken it.
	small, _ := newTestLimiter(t, 10*perMinute, 10)
	require.ErrorIs(t, small.Acquire(ctx, uuid.New(), PriorityRealtime, 50),
		ErrWeightExceedsBudget)
}

// A 429 is earned by the IP, so the penalty lands on the IP. Scoping it to the integration
// that tripped it would leave every other integration hammering an endpoint that has
// already started counting toward a ban.
func TestAPenaltyStopsEveryIntegration(t *testing.T) {
	ctx := context.Background()
	l, clock := newTestLimiter(t, perMinute, perMinute)
	a, b := uuid.New(), uuid.New()

	require.NoError(t, l.Acquire(ctx, a, PriorityRealtime, 1))
	l.Penalize(30 * time.Second)

	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() { errA <- l.Acquire(ctx, a, PriorityRealtime, 1) }()
	go func() { errB <- l.Acquire(ctx, b, PriorityRealtime, 1) }()
	waitUntilWaiting(t, l, 2)

	clock.Advance(29 * time.Second)
	requireStillWaiting(t, errA)
	requireStillWaiting(t, errB)

	clock.Advance(time.Second)
	requireServed(t, errA)
	requireServed(t, errB)
}

// The longest penalty wins. A second 429 arriving while the first is still in force must
// not shorten the wait, which is what a plain assignment would do.
func TestASecondPenaltyNeverShortensTheFirst(t *testing.T) {
	ctx := context.Background()
	l, clock := newTestLimiter(t, perMinute, perMinute)
	a := uuid.New()

	l.Penalize(30 * time.Second)
	l.Penalize(time.Second)

	errCh := make(chan error, 1)
	go func() { errCh <- l.Acquire(ctx, a, PriorityRealtime, 1) }()
	waitUntilWaiting(t, l, 1)

	clock.Advance(29 * time.Second)
	requireStillWaiting(t, errCh)
	clock.Advance(time.Second)
	requireServed(t, errCh)
}

// K24's priority order, and the only test that proves it. A backfill queued first must
// yield to a realtime request queued second: the user is watching the realtime one, and
// the backfill has hours to finish.
func TestRealtimeIsServedBeforeAnEarlierBackfill(t *testing.T) {
	ctx := context.Background()
	l, clock := newTestLimiter(t, perMinute, 10*perMinute)
	a := uuid.New()

	require.NoError(t, l.Acquire(ctx, a, PriorityBackfill, perMinute))

	backfill := make(chan error, 1)
	go func() { backfill <- l.Acquire(ctx, a, PriorityBackfill, 1) }()
	waitUntilWaiting(t, l, 1)

	realtime := make(chan error, 1)
	go func() { realtime <- l.Acquire(ctx, a, PriorityRealtime, 1) }()
	waitUntilWaiting(t, l, 2)

	// Exactly one unit of weight becomes available. It must go to the realtime caller.
	advanceOneToken(clock)
	requireServed(t, realtime)
	requireStillWaiting(t, backfill)

	advanceOneToken(clock)
	requireServed(t, backfill)
}

// Within one priority the order is the order of arrival, so a backfill cannot be starved
// by later backfills.
func TestWithinOnePriorityTheOrderIsFirstComeFirstServed(t *testing.T) {
	ctx := context.Background()
	l, clock := newTestLimiter(t, perMinute, 10*perMinute)
	a := uuid.New()

	require.NoError(t, l.Acquire(ctx, a, PriorityBackfill, perMinute))

	first := make(chan error, 1)
	go func() { first <- l.Acquire(ctx, a, PriorityBackfill, 1) }()
	waitUntilWaiting(t, l, 1)

	second := make(chan error, 1)
	go func() { second <- l.Acquire(ctx, a, PriorityBackfill, 1) }()
	waitUntilWaiting(t, l, 2)

	advanceOneToken(clock)
	requireServed(t, first)
	requireStillWaiting(t, second)
}

func TestReconcileOutranksBackfillAndYieldsToRealtime(t *testing.T) {
	require.Greater(t, PriorityRealtime, PriorityReconcile)
	require.Greater(t, PriorityReconcile, PriorityBackfill)
}

// A cancelled caller must return promptly and must leave the budget exactly as it found
// it. Charging a caller that never made a request is how a budget drains with nothing to
// show for it.
func TestACancelledCallerReturnsAndConsumesNothing(t *testing.T) {
	l, clock := newTestLimiter(t, perMinute, perMinute)
	a := uuid.New()

	require.NoError(t, l.Acquire(context.Background(), a, PriorityBackfill, perMinute))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Acquire(ctx, a, PriorityBackfill, 1) }()
	waitUntilWaiting(t, l, 1)

	cancel()
	require.ErrorIs(t, <-errCh, context.Canceled)
	require.Zero(t, l.Waiting(), "a cancelled caller must leave the queue")

	// The unit of weight it was waiting for is still there for the next caller.
	advanceOneToken(clock)
	require.NoError(t, l.Acquire(context.Background(), a, PriorityBackfill, 1))
}

// An already-cancelled context must not spend budget even though the budget is full: the
// caller is not going to make the request.
func TestAnAlreadyCancelledContextSpendsNothing(t *testing.T) {
	l, _ := newTestLimiter(t, perMinute, perMinute)
	a := uuid.New()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, l.Acquire(ctx, a, PriorityBackfill, 1), context.Canceled)

	// The whole budget is still available, so nothing was charged.
	require.NoError(t, l.Acquire(context.Background(), a, PriorityBackfill, perMinute))
}

// The ceiling comes from exchangeInfo at connect time (K24). A zero is refused rather than
// defaulted: a hardcoded number is one that will be wrong after Binance changes it, and we
// would find out from the ban.
func TestAMissingCeilingIsRefusedRatherThanDefaulted(t *testing.T) {
	clock := newFakeClock()
	for name, cfg := range map[string]Config{
		"no shared ceiling":          {SharedPerMinute: 0, PerIntegrationPerMinute: 100},
		"no per-integration ceiling": {SharedPerMinute: 100, PerIntegrationPerMinute: 0},
		"both absent":                {},
		"negative":                   {SharedPerMinute: -1, PerIntegrationPerMinute: 100},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(cfg, clock)
			require.ErrorIs(t, err, ErrNoCeiling)
		})
	}
}

// Weight is what varies between endpoints, so the interface takes weight rather than a
// call count: myTrades at 20 and exchangeInfo at 20 must not look like two cheap calls.
func TestWeightIsChargedNotCalls(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLimiter(t, perMinute, perMinute)
	a := uuid.New()

	require.NoError(t, l.Acquire(ctx, a, PriorityBackfill, 60))
	require.NoError(t, l.Acquire(ctx, a, PriorityBackfill, 40))

	errCh := make(chan error, 1)
	go func() { errCh <- l.Acquire(ctx, a, PriorityBackfill, 1) }()
	waitUntilWaiting(t, l, 1)
	requireStillWaiting(t, errCh)
}

func TestZeroAndNegativeWeightAreRejected(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLimiter(t, perMinute, perMinute)

	require.ErrorIs(t, l.Acquire(ctx, uuid.New(), PriorityRealtime, 0), ErrInvalidWeight)
	require.ErrorIs(t, l.Acquire(ctx, uuid.New(), PriorityRealtime, -5), ErrInvalidWeight)
}
