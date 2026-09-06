package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/asset"
	"github.com/Contictus/plimsoll/backend/internal/backfill"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
)

// This file is the seam between the exchange adapter and the supervisor. It holds no policy
// of its own: what an event means is the normalizer's decision, and how far back to read is
// the backfill's. What it does hold is the list of events we knowingly ignore, which is a
// decision, and is written down rather than left as a fallthrough.

// unmodelledEvents are user data events that are real, understood, and move nothing the
// ledger models today.
//
// Named rather than defaulted. A fallthrough that ignored anything it did not recognise
// would swallow a new kind of balance movement the day Binance introduces one, and a
// missing acquisition poisons the cost basis permanently (K26). Everything absent from this
// map and not a fill stops the ingest.
//
// Names quoted from user-data-stream.md on 2026-09-04.
var unmodelledEvents = map[string]string{
	// Balances are a projection of the ledger, not an input to it (L3). Recording an
	// exchange's balance snapshot as an event would put a number in the ledger that no
	// fold produced.
	"outboundAccountPosition": "a balance snapshot, which is a projection rather than an event",

	// A deposit or withdrawal arriving. It says how much moved but not what caused it, and
	// the deposit endpoint reports the same movement with an id we can deduplicate on --
	// so this is skipped and the REST walk is the source. The consequence is recorded as a
	// known gap: a deposit made while the stream is healthy waits for the next walk.
	"balanceUpdate": "a balance delta; the deposit walk is the source of record for these",

	"externalLockUpdate":    "collateral locked or unlocked by another system; no ledger effect",
	"eventStreamTerminated": "the subscription ended; the stream reports that as a gap",
	"listStatus":            "the status of an order list; its fills arrive as executionReport",
}

// StreamIngester normalizes one live user data event into canonical events.
type StreamIngester struct {
	Resolver binance.InstrumentResolver
	Context  binance.IngestContext
}

// Ingest returns the events one frame produced, which is normally none: most execution
// reports are orders being placed or cancelled. Returning none is deliberately not an
// error, or the ingest would stop on the first order the account placed.
func (i StreamIngester) Ingest(
	ctx context.Context, raw json.RawMessage,
) ([]ledger.Event, error) {
	eventType, err := userDataEventType(raw)
	if err != nil {
		return nil, err
	}

	if eventType != "executionReport" {
		if _, known := unmodelledEvents[eventType]; known {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"worker: user data event %q is neither a fill nor on the list of events we"+
				" knowingly ignore, and guessing which it is would either lose a balance"+
				" movement or invent one", eventType)
	}

	event, err := binance.NormalizeStreamExecutionReport(ctx, i.Resolver, i.Context, raw)
	if errors.Is(err, binance.ErrNotAFill) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []ledger.Event{event}, nil
}

// userDataEventType reads the "e" field, and reads it by exact key.
//
// A tagged struct would not do. encoding/json falls back to case-insensitive matching when
// no exact tag matches, and every one of these payloads carries both "e" (the event type, a
// string) and "E" (the event time, a number) -- so a struct tagged `json:"e"` is handed the
// timestamp and fails, or worse, succeeds on some other pair. The normalizer learned this
// the same way; it is written down twice because the trap is invisible at the call site.
func userDataEventType(raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", fmt.Errorf("worker: user data frame is not an object: %w", err)
	}
	value, ok := fields["e"]
	if !ok {
		return "", errors.New("worker: user data frame names no event type")
	}
	var eventType string
	if err := json.Unmarshal(value, &eventType); err != nil {
		return "", fmt.Errorf("worker: user data event type is not a string: %w", err)
	}
	return eventType, nil
}

// WindowResyncer replays a gap window over REST. It is the supervisor's Resyncer, and it
// carries the symbols to replay because there is no endpoint that returns trades across
// symbols (F4) -- the same reason discovery exists.
type WindowResyncer struct {
	Deps    backfill.Deps
	Target  backfill.Target
	Symbols func(ctx context.Context) ([]string, error)
}

// Resync reads the window and appends what it finds. The supervisor has already split the
// gap into windows the venue will answer; ResyncWindow refuses a wider one anyway, because
// a rejected replay reads as an empty one.
func (r WindowResyncer) Resync(ctx context.Context, from, to time.Time) error {
	symbols, err := r.Symbols(ctx)
	if err != nil {
		return fmt.Errorf("worker: symbols to resync: %w", err)
	}
	return backfill.ResyncWindow(ctx, r.Deps, r.Target, symbols, from, to)
}

// TradedSymbols returns the symbols an account is known to have traded, from the scopes
// discovery opened. It is what a resync replays: probing every symbol again for a
// five-minute gap would cost the whole discovery sweep for nothing.
func TradedSymbols(d backfill.Deps, t backfill.Target) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		scopes, err := backfill.Scopes(ctx, d, t, backfill.ScopeTrades(""))
		if err != nil {
			return nil, err
		}
		symbols := make([]string, 0, len(scopes))
		for _, scope := range scopes {
			if symbol, ok := backfill.SymbolOf(scope.Scope); ok {
				symbols = append(symbols, symbol)
			}
		}
		return symbols, nil
	}
}

// Registry resolves exchange symbols and coin tickers through the canonical registries.
//
// It takes the pool rather than a transaction because the alias tables are reference data:
// no RLS, no per-account rows, and no write grant for the app role (00004, 00005). A lookup
// therefore needs no account bound, and forcing one would open a transaction per event for
// nothing.
type Registry struct {
	DB store.DBTX
	// Exchange is the alias namespace, e.g. "binance". Part of the key, not a detail:
	// two venues may use one string for different instruments.
	Exchange string
}

// Instrument resolves a trading pair as of the event's own time, never time.Now() (L8).
func (r Registry) Instrument(
	ctx context.Context, market instrument.Market, symbol string, at time.Time,
) (int64, error) {
	return instrument.Resolve(ctx, store.New(r.DB), r.Exchange, market, symbol, at)
}

// Asset resolves a coin ticker as of the event's own time, for the same reason.
func (r Registry) Asset(ctx context.Context, symbol string, at time.Time) (int64, error) {
	return asset.Resolve(ctx, store.New(r.DB), r.Exchange, symbol, at)
}
