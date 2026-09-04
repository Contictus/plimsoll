// Package worker owns the ingestion process: who is allowed to write for an integration,
// and what state that writing is in.
//
// The lease is the first half. L6 says a projection advances per integration_id under a
// single-writer lease; with the backfill and the live stream both writing, that lease stops
// being a design note and becomes the thing that keeps two folds from racing on one cursor.
package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNoOwner is returned when a lease operation is asked to act for an unnamed worker. An
// empty owner would match every row the schema allows and none it does not, which is the
// one identity that must never be usable.
var ErrNoOwner = errors.New("worker: lease operations need a named owner")

// ErrTTLTooShort means the caller asked for a lease shorter than the statement can express.
// The interval is built from whole seconds, so anything under one rounds to zero and would
// expire before it was acquired -- refused here, where the caller can still see what it
// asked for, rather than in the schema's CHECK.
var ErrTTLTooShort = errors.New("worker: a lease ttl below one second cannot be expressed")

// minTTL is the shortest lease the statement can express.
const minTTL = time.Second

func ttlSeconds(ttl time.Duration) (int32, error) {
	if ttl < minTTL {
		return 0, fmt.Errorf("%w: got %s", ErrTTLTooShort, ttl)
	}
	return int32(ttl / time.Second), nil
}

// Claim takes the lease for an integration, or reports that someone else holds it. Not
// holding the lease is a normal outcome, not an error: on a fleet of workers most claims
// lose, and a loss that arrived as an error would be logged as a failure every cycle.
//
// The holder re-claiming is a renewal rather than a conflict, so a worker restarting its
// own loop does not have to wait out its own lease.
//
// ttl is how long the lease survives without a heartbeat. It is measured by the database's
// clock, never the caller's: two workers with skewed clocks would otherwise disagree about
// whether a lease had expired, and the one running fast would take a lease still held.
func Claim(
	ctx context.Context,
	db tenancy.Beginner,
	accountID, integrationID uuid.UUID,
	ownerID string,
	ttl time.Duration,
) (bool, error) {
	if ownerID == "" {
		return false, ErrNoOwner
	}
	secs, err := ttlSeconds(ttl)
	if err != nil {
		return false, err
	}

	held := false
	err = tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		_, err := q.ClaimIntegrationLease(ctx, store.ClaimIntegrationLeaseParams{
			AccountID:     accountID,
			IntegrationID: integrationID,
			OwnerID:       ownerID,
			TtlSeconds:    secs,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// The conflict clause matched nothing: the lease is live and belongs to
			// someone else.
			return nil
		}
		if err != nil {
			return fmt.Errorf("worker: claim lease on %s: %w", integrationID, err)
		}
		held = true
		return nil
	})
	return held, err
}

// Heartbeat extends a lease the caller still holds and reports whether it still holds one.
// A false return means the lease is gone -- expired, or taken -- and the caller must stop
// writing immediately rather than finish the batch it was on.
func Heartbeat(
	ctx context.Context,
	db tenancy.Beginner,
	accountID, integrationID uuid.UUID,
	ownerID string,
	ttl time.Duration,
) (bool, error) {
	if ownerID == "" {
		return false, ErrNoOwner
	}
	secs, err := ttlSeconds(ttl)
	if err != nil {
		return false, err
	}

	alive := false
	err = tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		_, err := q.HeartbeatIntegrationLease(ctx, store.HeartbeatIntegrationLeaseParams{
			AccountID:     accountID,
			IntegrationID: integrationID,
			OwnerID:       ownerID,
			TtlSeconds:    secs,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("worker: heartbeat lease on %s: %w", integrationID, err)
		}
		alive = true
		return nil
	})
	return alive, err
}

// Release gives up a lease on a clean shutdown, so the next worker does not have to wait
// out a full TTL for an integration nobody is working on. Releasing a lease held by someone
// else does nothing and is not an error: the outcome a caller cares about is "I no longer
// hold it", which is true either way.
func Release(
	ctx context.Context,
	db tenancy.Beginner,
	accountID, integrationID uuid.UUID,
	ownerID string,
) error {
	if ownerID == "" {
		return ErrNoOwner
	}
	return tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		if _, err := q.ReleaseIntegrationLease(ctx, store.ReleaseIntegrationLeaseParams{
			AccountID:     accountID,
			IntegrationID: integrationID,
			OwnerID:       ownerID,
		}); err != nil {
			return fmt.Errorf("worker: release lease on %s: %w", integrationID, err)
		}
		return nil
	})
}
