//go:build integration

package worker_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

const leaseTTL = time.Minute

func ownerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func seedIntegration(t *testing.T) (accountID, integrationID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	accountID, integrationID = uuid.New(), uuid.New()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, email) VALUES ($1, $2)`,
			accountID, "worker-"+accountID.String()+"@example.test"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO integrations (id, account_id, exchange, label)
			 VALUES ($1, $2, 'binance', 'test')`, integrationID, accountID)
		return err
	}))
	return accountID, integrationID
}

// expireLease backdates a held lease, which is what a worker that died looks like from the
// outside. Done in SQL rather than by sleeping: the lease's whole clock is the database's,
// so moving the row is both the honest setup and the deterministic one.
func expireLease(t *testing.T, accountID, integrationID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE integration_leases
			    SET acquired_at = now() - interval '2 hours',
			        expires_at  = now() - interval '1 hour'
			  WHERE integration_id = $1`, integrationID)
		if err != nil {
			return err
		}
		require.EqualValues(t, 1, tag.RowsAffected(), "no lease to expire")
		return nil
	}))
}

// THE TEST THIS TABLE EXISTS FOR (L6, K20).
//
// Two writers on one integration means two folds racing on one cursor: the loser's events
// land behind the winner's cursor, are never read, and the position stays permanently wrong
// with nothing in freshness to say so. The claim is a single INSERT ... ON CONFLICT ... and
// its entire value is that it is atomic, so it is tested with real concurrent transactions
// rather than with two sequential calls that would pass under any implementation.
func TestExactlyOneOfManyRacingWorkersWinsTheLease(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	const workers = 8
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	done.Add(workers)

	won := make([]bool, workers)
	errs := make([]error, workers)
	for i := range workers {
		go func() {
			defer done.Done()
			start.Wait()
			won[i], errs[i] = worker.Claim(
				ctx, pool, accountID, integrationID, uuid.NewString(), leaseTTL)
		}()
	}
	start.Done()
	done.Wait()

	winners := 0
	for i := range workers {
		require.NoError(t, errs[i])
		if won[i] {
			winners++
		}
	}
	require.Equal(t, 1, winners, "exactly one worker may hold an integration")
}

func TestALiveLeaseIsNotClaimableByAnother(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	held, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-a", leaseTTL)
	require.NoError(t, err)
	require.True(t, held)

	stolen, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-b", leaseTTL)
	require.NoError(t, err)
	require.False(t, stolen, "a live lease is not available, and saying so is not an error")

	// The holder re-claiming is a renewal, not a second holder. A worker that restarts its
	// loop must not have to wait out its own lease.
	renewed, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-a", leaseTTL)
	require.NoError(t, err)
	require.True(t, renewed)
}

// A worker that died holds a lease nobody will ever release. If an expired lease were not
// claimable the integration would stop ingesting until a human noticed, which is the
// failure mode the expiry exists to remove.
func TestAnExpiredLeaseIsClaimable(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	held, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-a", leaseTTL)
	require.NoError(t, err)
	require.True(t, held)

	expireLease(t, accountID, integrationID)

	taken, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-b", leaseTTL)
	require.NoError(t, err)
	require.True(t, taken, "a lease whose holder is gone must not strand the integration")
}

func TestAReleasedLeaseIsImmediatelyClaimable(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	_, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-a", leaseTTL)
	require.NoError(t, err)
	require.NoError(t, worker.Release(ctx, pool, accountID, integrationID, "worker-a"))

	taken, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-b", leaseTTL)
	require.NoError(t, err)
	require.True(t, taken, "a clean shutdown must not cost the next worker a full TTL")
}

// Releasing someone else's lease would hand the integration to a third process while the
// second was still writing -- the two-writer race, arrived at from the other direction.
func TestALeaseCanOnlyBeReleasedByItsOwner(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	_, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-a", leaseTTL)
	require.NoError(t, err)
	require.NoError(t, worker.Release(ctx, pool, accountID, integrationID, "worker-b"))

	stolen, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-b", leaseTTL)
	require.NoError(t, err)
	require.False(t, stolen, "worker-a still holds it")
}

func TestHeartbeatKeepsTheLeaseAndOnlyForItsOwner(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	_, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-a", leaseTTL)
	require.NoError(t, err)

	alive, err := worker.Heartbeat(ctx, pool, accountID, integrationID, "worker-a", leaseTTL)
	require.NoError(t, err)
	require.True(t, alive)

	impostor, err := worker.Heartbeat(ctx, pool, accountID, integrationID, "worker-b", leaseTTL)
	require.NoError(t, err)
	require.False(t, impostor, "a heartbeat is not a way to take a lease")
}

// A worker that stopped heartbeating long enough for its lease to lapse has lost it,
// whether or not anyone else has taken it yet. Letting it heartbeat its way back is exactly
// the two-writer window the lease exists to close: another worker may already be mid-claim.
func TestAWorkerThatStopsHeartbeatingLosesTheLease(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	_, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-a", leaseTTL)
	require.NoError(t, err)
	expireLease(t, accountID, integrationID)

	alive, err := worker.Heartbeat(ctx, pool, accountID, integrationID, "worker-a", leaseTTL)
	require.NoError(t, err)
	require.False(t, alive, "an expired lease is lost, not renewable")
}

// waitForALockWaiter blocks until an app-role backend is waiting on a lock, so the
// interleaving below is the blocking one rather than an accidental sequential run. Polled
// rather than slept: a sleep long enough to be reliable is long enough to be slow, and one
// short enough to be fast is a flake.
//
// Polled through the app pool on purpose. pg_stat_activity hides other roles' rows, and the
// backend we are waiting for is the app role's -- asking as the owner returns
// "<insufficient privilege>" and the wait never resolves.
func waitForALockWaiter(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pool := appPool(t)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database()
			    AND state = 'active' AND wait_event_type = 'Lock'`).Scan(&waiting))
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no transaction ever blocked on the lease row; the interleaving was not exercised")
}

// THE ATOMICITY TEST (K20).
//
// It goes at the statement rather than at worker.Claim, because that is where the claim's
// whole value is: the guard and the write are one INSERT ... ON CONFLICT DO UPDATE, so a
// second transaction blocks on the row lock and then re-evaluates its WHERE against the
// winner's committed row.
//
// Racing goroutines are not enough on their own -- they can serialize, and then a
// read-then-write implementation passes. Here the losing transaction is *made* to block
// while the winner is still open, which is exactly the window a read-then-write leaves open
// and an atomic statement does not.
func TestTheClaimBlocksAndLosesRatherThanRacing(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	winner, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = winner.Rollback(ctx) }()
	_, err = winner.Exec(ctx, `SELECT set_config('app.account_id', $1, true)`, accountID.String())
	require.NoError(t, err)

	_, err = store.New(winner).ClaimIntegrationLease(ctx, store.ClaimIntegrationLeaseParams{
		AccountID: accountID, IntegrationID: integrationID,
		OwnerID: "worker-a", TtlSeconds: int32(leaseTTL / time.Second),
	})
	require.NoError(t, err, "the first claim takes the row lock and holds it uncommitted")

	loserErr := make(chan error, 1)
	go func() {
		loserErr <- tenancy.InTx(ctx, pool, accountID, func(q *store.Queries) error {
			_, err := q.ClaimIntegrationLease(ctx, store.ClaimIntegrationLeaseParams{
				AccountID: accountID, IntegrationID: integrationID,
				OwnerID: "worker-b", TtlSeconds: int32(leaseTTL / time.Second),
			})
			return err
		})
	}()

	waitForALockWaiter(t)
	require.NoError(t, winner.Commit(ctx))

	select {
	case err := <-loserErr:
		require.ErrorIs(t, err, pgx.ErrNoRows,
			"the blocked transaction must re-evaluate against the committed row and lose")
	case <-time.After(10 * time.Second):
		t.Fatal("the losing claim never returned")
	}

	var owner string
	require.NoError(t, tenancy.InTxRaw(ctx, ownerPool(t), accountID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT owner_id FROM integration_leases WHERE integration_id = $1`,
			integrationID).Scan(&owner)
	}))
	require.Equal(t, "worker-a", owner, "the lease must still belong to the transaction that won")
}

// A lease is measured in whole seconds, because that is what the statement asks the
// database for. Sub-second is not a shorter lease, it is a lease that rounds to zero and
// expires before it is acquired -- which the schema refuses, and which is better refused
// here where the caller can see what it asked for.
//
// Seconds are an integer for a second reason: sqlc emits float64 for a double precision
// parameter, and a float64 anywhere in generated store code is a guard L1 keeps armed by
// refusing all of them rather than by arguing about which ones are money.
func TestALeaseTooShortToExpressIsRefused(t *testing.T) {
	ctx := context.Background()
	accountID, integrationID := seedIntegration(t)
	pool := appPool(t)

	_, err := worker.Claim(ctx, pool, accountID, integrationID, "worker-a", 900*time.Millisecond)
	require.ErrorIs(t, err, worker.ErrTTLTooShort)

	_, err = worker.Heartbeat(ctx, pool, accountID, integrationID, "worker-a", 0)
	require.ErrorIs(t, err, worker.ErrTTLTooShort)
}
