// Package events is the live-update path: the worker says "this changed" and a browser is
// told to re-read.
//
// It fans out over Postgres LISTEN/NOTIFY rather than the Redis K28 reserved for it (K51).
// M4 put prices in Postgres and never needed Redis's other job, and one job does not earn a
// container. The day a second api replica exists this becomes wrong in a specific and visible
// way -- each replica only hears what its own connection listens for -- and that is the day
// Redis earns it.
package events

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The topics a subscriber can be told about. A closed set, because a topic is a contract with
// a client: an unrecognized one is a page that never refreshes and never says why.
const (
	TopicPortfolio = "portfolio"
	TopicPositions = "positions"
	TopicRisk      = "risk"
	TopicAlerts    = "alerts"
)

// SubscriberBuffer is how many notifications one client may fall behind by before it is
// dropped. Small on purpose: these are hints, not data, and a client that missed three of
// them needs exactly one re-read to be correct again.
const SubscriberBuffer = 16

// Message is one hint. It carries no numbers -- deliberately (K51).
type Message struct {
	Topic string    `json:"topic"`
	At    time.Time `json:"at"`
}

// Publish tells this account's subscribers that something changed.
//
// q must come from the transaction that made the change: NOTIFY is delivered on commit, so a
// subscriber is never told to re-read something that then rolls back. Published outside the
// transaction it would be a race with a schedule.
func Publish(ctx context.Context, q *store.Queries, accountID uuid.UUID, topic string) error {
	payload, err := json.Marshal(Message{Topic: topic, At: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("events: encode %s hint: %w", topic, err)
	}
	if err := q.NotifyAccount(ctx, store.NotifyAccountParams{
		Channel: channelFor(accountID), Payload: string(payload),
	}); err != nil {
		return fmt.Errorf("events: notify %s: %w", accountID, err)
	}
	return nil
}

// channelFor is one channel per account, so a subscriber cannot be handed another account's
// notifications by the transport itself. Hex only, and 41 characters -- inside Postgres'
// 63-byte identifier limit with room to spare.
func channelFor(accountID uuid.UUID) string {
	return "plimsoll_" + hex.EncodeToString(accountID[:])
}

// Subscriber is what the HTTP layer is given. An interface, and PoolSubscriber below is the
// only implementation, because holding a connection is exactly what the depguard rule keeps
// out of the request path (K15): httpapi names this and never a pool.
type Subscriber interface {
	Subscribe(ctx context.Context, accountID uuid.UUID) (<-chan Message, error)
}

// PoolSubscriber subscribes out of a connection pool.
type PoolSubscriber struct{ Pool *pgxpool.Pool }

// Subscribe opens a live subscription for one account.
func (p PoolSubscriber) Subscribe(
	ctx context.Context, accountID uuid.UUID,
) (<-chan Message, error) {
	return Subscribe(ctx, p.Pool, accountID)
}

// Subscribe opens a live subscription for one account. The returned channel is closed when
// ctx is cancelled, when the connection drops, or when the subscriber falls too far behind.
//
// It takes a connection out of the pool and holds it: LISTEN is a property of a session, not
// of a statement. That is the cost of this design, and it is what will make Redis the right
// answer the day there is more than one api replica (K51).
func Subscribe(
	ctx context.Context, pool *pgxpool.Pool, accountID uuid.UUID,
) (<-chan Message, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("events: acquire a connection for %s: %w", accountID, err)
	}

	channel := channelFor(accountID)
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		conn.Release()
		return nil, fmt.Errorf("events: listen for %s: %w", accountID, err)
	}

	out := make(chan Message, SubscriberBuffer)
	go func() {
		defer close(out)
		// Released rather than reused: a connection that has LISTENed is not clean, and
		// handing it back to the pool still listening would deliver this account's hints to
		// whoever picked it up next.
		defer conn.Release()

		for {
			notification, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				// Cancellation and a dropped connection end the subscription the same way:
				// the client reconnects, and one re-read makes it correct again.
				return
			}
			var m Message
			if err := json.Unmarshal([]byte(notification.Payload), &m); err != nil {
				continue
			}

			select {
			case out <- m:
			default:
				// Dropped rather than blocked. A browser tab on a sleeping laptop must not
				// be able to stall the notification path for everyone else on the process,
				// and a dropped client is recoverable by reconnecting while a stalled
				// publisher is recoverable by nothing.
				return
			}
		}
	}()
	return out, nil
}
