package worker_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/worker"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type stubResolver struct{ id int64 }

func (s stubResolver) Instrument(
	_ context.Context, _ instrument.Market, _ string, _ time.Time,
) (int64, error) {
	return s.id, nil
}

func (s stubResolver) Asset(_ context.Context, _ string, _ time.Time) (int64, error) {
	return s.id, nil
}

func streamEvent(t *testing.T, name string) json.RawMessage {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "binance", name))
	require.NoError(t, err)
	var envelope struct {
		Event json.RawMessage `json:"event"`
	}
	require.NoError(t, json.Unmarshal(blob, &envelope))
	return envelope.Event
}

func newIngester() worker.StreamIngester {
	return worker.StreamIngester{
		Resolver: stubResolver{id: 7},
		Context: binance.IngestContext{
			AccountID:     uuid.New(),
			IntegrationID: uuid.New(),
			Source:        binance.SourceStream,
		},
	}
}

func TestTheIngesterTurnsAFillIntoOneEvent(t *testing.T) {
	events, err := newIngester().Ingest(
		context.Background(), streamEvent(t, "execution_report_trade_bnbbtc.json"))
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, ledger.TypeTrade, events[0].EventType)
	require.Equal(t, binance.SpotTradeID("BNBBTC", 28457), events[0].VenueEventID)
}

// Most execution reports are orders being placed, cancelled or expiring. They are real,
// understood events that move no position, so the supervisor writes nothing -- and that is
// deliberately not an error, or the ingest would stop on the first order anyone placed.
func TestAnOrderThatIsNotAFillProducesNoEventAndNoError(t *testing.T) {
	events, err := newIngester().Ingest(
		context.Background(), streamEvent(t, "execution_report_new.json"))
	require.NoError(t, err)
	require.Empty(t, events)
}

// An event type we do not model is loud, not silent. Guessing would either drop a trade the
// day Binance adds a way to report one, or record something that never happened (K26).
func TestAnUnknownEventStopsTheIngestRatherThanBeingSkipped(t *testing.T) {
	_, err := newIngester().Ingest(context.Background(),
		json.RawMessage(`{"e":"executionReport","x":"SOMETHING_NEW","s":"BNBBTC","t":1}`))
	require.ErrorIs(t, err, binance.ErrUnknownExecutionType)
}

// Events the ledger does not model yet -- a balance update, an external lock -- are skipped
// by name. Skipping by name rather than by falling through means the list of what we
// knowingly ignore is written down, and an event outside it still stops the ingest.
func TestKnownButUnmodelledEventsAreSkippedByName(t *testing.T) {
	for _, payload := range []string{
		`{"e":"outboundAccountPosition","E":1564034571105,"u":1564034571073,"B":[]}`,
		`{"e":"balanceUpdate","E":1573200697110,"a":"BTC","d":"100.0","T":1573200697068}`,
		`{"e":"externalLockUpdate","E":1581557507324,"a":"NEO","d":"10.0","T":1581557507268}`,
		`{"e":"eventStreamTerminated","E":1728973001334}`,
		`{"e":"listStatus","E":1564035303637,"s":"ETHBTC"}`,
	} {
		events, err := newIngester().Ingest(context.Background(), json.RawMessage(payload))
		require.NoError(t, err, "payload %s", payload)
		require.Empty(t, events)
	}
}

// An event whose type we have never seen is not on the skip list and is not a fill, so it
// stops the ingest. Silence here is how a new kind of balance movement goes unrecorded for
// months.
// The "e"/"E" pair is why the type is read by exact key. encoding/json matches
// case-insensitively when no exact tag matches, so a struct tagged `json:"e"` is handed
// "E" -- the event time, a number -- on every payload that carries both.
func TestTheEventTypeIsReadCaseSensitively(t *testing.T) {
	events, err := newIngester().Ingest(context.Background(),
		json.RawMessage(`{"e":"eventStreamTerminated","E":1728973001334}`))
	require.NoError(t, err, "E must not be read as the event type")
	require.Empty(t, events)
}

func TestAnUnrecognisedEventTypeStopsTheIngest(t *testing.T) {
	_, err := newIngester().Ingest(context.Background(),
		json.RawMessage(`{"e":"somethingBinanceAddedLater","E":1}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "somethingBinanceAddedLater")
}
