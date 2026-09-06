package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
)

// Status is one integration as a reader sees it: how it is configured, and what the worker
// that owns it last said about itself.
//
// Reported is the field that matters most. False means no worker has ever published for
// this integration, and a zero State is not "connecting" -- it is "nobody is ingesting
// this". Collapsing the two would let an integration nobody runs look like one that is
// about to start.
type Status struct {
	IntegrationID uuid.UUID
	Exchange      string
	Label         string

	// Configured is integrations.status: active, paused or revoked. A paused integration
	// with no worker is expected; an active one with no worker is a fault.
	Configured string

	Reported  bool
	State     State
	OwnerID   string
	Since     time.Time
	UpdatedAt time.Time
}

// Stale reports whether the worker has stopped saying anything, judged against the lease
// TTL rather than a constant: the lease is what makes a worker the writer, so a report
// older than one lease is a report from a worker that no longer holds the integration.
//
// A stale report is worse than any state it names. "Live, as of forty minutes ago" is the
// sentence L11 exists to keep out of a response.
func (s Status) Stale(now time.Time, leaseTTL time.Duration) bool {
	if !s.Reported {
		return true
	}
	return now.Sub(s.UpdatedAt) > leaseTTL
}

// Publish records what one worker is doing. It is called inside the caller's own
// transaction so the lease can be checked in the same one: a worker that lost its lease
// must not go on describing an integration it no longer writes.
//
// since moves only when the state changes; the SQL enforces that, not this function,
// because a heartbeat republishing the same state must not keep resetting how long it has
// been that way.
func Publish(
	ctx context.Context,
	q *store.Queries,
	accountID, integrationID uuid.UUID,
	ownerID string,
	state State,
	since time.Time,
) error {
	if err := q.UpsertIntegrationStatus(ctx, store.UpsertIntegrationStatusParams{
		AccountID:     accountID,
		IntegrationID: integrationID,
		State:         string(state),
		OwnerID:       ownerID,
		Since:         since,
	}); err != nil {
		return fmt.Errorf("ingest: publish %s status for %s: %w", state, integrationID, err)
	}
	return nil
}

// ReadStatus returns every integration the account has, reported or not. The API calls it
// to build the freshness of a portfolio response, which is why it lists integrations rather
// than statuses: an integration nobody is ingesting contributes the loudest reason of all,
// and a query over the status table alone could not see it.
func ReadStatus(
	ctx context.Context, db tenancy.Beginner, accountID uuid.UUID,
) ([]Status, error) {
	var out []Status
	err := tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		var err error
		out, err = StatusIn(ctx, q, accountID)
		return err
	})
	return out, err
}

// StatusIn is ReadStatus inside a transaction the caller already owns. A portfolio response
// reads the positions and the statuses that qualify them; doing that in two transactions
// could show a position from after the status describing it, which is a response that
// contradicts itself (L10).
func StatusIn(ctx context.Context, q *store.Queries, accountID uuid.UUID) ([]Status, error) {
	rows, err := q.ListIntegrationStatus(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("ingest: read status for account %s: %w", accountID, err)
	}

	out := make([]Status, 0, len(rows))
	for _, r := range rows {
		s := Status{
			IntegrationID: r.IntegrationID,
			Exchange:      r.Exchange,
			Label:         r.Label,
			Configured:    r.ConfiguredStatus,
		}
		// A NULL state is the LEFT JOIN finding no report at all, which is not a state --
		// it is the absence of one, and Reported is what keeps the two distinguishable.
		if r.State != nil {
			s.Reported = true
			s.State = State(*r.State)
			if r.OwnerID != nil {
				s.OwnerID = *r.OwnerID
			}
			if r.Since != nil {
				s.Since = *r.Since
			}
			if r.UpdatedAt != nil {
				s.UpdatedAt = *r.UpdatedAt
			}
		}
		out = append(out, s)
	}
	return out, nil
}
