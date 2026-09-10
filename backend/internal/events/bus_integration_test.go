//go:build integration

package events_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/events"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func seedAccount(t *testing.T) uuid.UUID {
	t.Helper()
	owner, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	defer owner.Close()

	id := uuid.New()
	_, err = owner.Exec(context.Background(),
		`INSERT INTO accounts (id, email) VALUES ($1, $2)`,
		id, "events-"+id.String()+"@example.test")
	require.NoError(t, err)
	return id
}

// publish takes the pool rather than opening one. Opening a pool per call is how the slow
// subscriber test below came to open sixty-four of them and exhaust Postgres' connection
// slots for every other package running beside it.
func publish(t *testing.T, pool *pgxpool.Pool, accountID uuid.UUID, topic string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, tenancy.InTx(ctx, pool, accountID, func(q *store.Queries) error {
		return events.Publish(ctx, q, accountID, topic)
	}))
}

func waitFor(t *testing.T, ch <-chan events.Message) events.Message {
	t.Helper()
	select {
	case m, ok := <-ch:
		require.True(t, ok, "the subscription closed before delivering anything")
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
		return events.Message{}
	}
}

// A subscriber receives its own account's events and no others.
//
// The channel name carries the account and the payload carries a TOPIC, never numbers: a
// client is told "portfolio changed" and re-reads through the authenticated endpoint. Pushing
// the numbers down here would put a second, unversioned copy of the API contract on the wire
// -- one no freshness envelope travels with, and a second place for a tenancy mistake to
// live (K51).
func TestASubscriberReceivesOnlyItsOwnAccountsEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := appPool(t)
	mine, theirs := seedAccount(t), seedAccount(t)
	stream, err := events.Subscribe(ctx, pool, mine)
	require.NoError(t, err)

	publish(t, pool, theirs, events.TopicPortfolio)
	publish(t, pool, mine, events.TopicRisk)

	got := waitFor(t, stream)
	require.Equal(t, events.TopicRisk, got.Topic,
		"the first event to arrive was another account's")

	select {
	case extra := <-stream:
		t.Fatalf("another account's event was delivered: %v", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// A slow client is dropped; it never blocks the publisher. One browser tab left open on a
// laptop that went to sleep must not be able to stall the notification path for everyone
// else on the process.
func TestASlowSubscriberIsDroppedRatherThanBlockingThePublisher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := appPool(t)
	accountID := seedAccount(t)
	stream, err := events.Subscribe(ctx, pool, accountID)
	require.NoError(t, err)

	// Nothing reads `stream` while this loop runs, so its buffer fills and overflows.
	for i := 0; i < events.SubscriberBuffer*4; i++ {
		publish(t, pool, accountID, events.TopicPositions)
	}

	// The subscription closes rather than wedging: a dropped client is recoverable by
	// reconnecting, and a stalled publisher is not recoverable by anything.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-stream:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the slow subscriber was neither served nor dropped")
		}
	}
}

// A disconnect frees the subscription. Without this the process leaks one goroutine and one
// connection per reconnect, and a browser reconnecting every few seconds is a leak with a
// schedule.
func TestCancellingASubscriptionClosesIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	accountID := seedAccount(t)
	stream, err := events.Subscribe(ctx, appPool(t), accountID)
	require.NoError(t, err)

	cancel()
	select {
	case _, ok := <-stream:
		require.False(t, ok, "the channel yielded a value after cancellation")
	case <-time.After(5 * time.Second):
		t.Fatal("the subscription outlived its context")
	}
}
