package marketdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// eventMiniTicker and eventServerShutdown are the two event types this decoder has a rule
// for. Anything else is refused rather than skipped: skipping is how a new kind of push
// becomes silence, and silence is the worst possible failure (L11).
const (
	eventMiniTicker     = "24hrMiniTicker"
	eventServerShutdown = "serverShutdown"
)

// ErrUnknownEvent means the venue pushed something this decoder has no rule for.
var ErrUnknownEvent = errors.New("marketdata: no rule for this event type")

// Frame is what one push turned out to be.
//
// Shutdown is separate from Quotes rather than an error, because it is not a failure: F8
// records that the market stream announces its own shutdown, which the user-data stream has
// no equivalent of. Recognising it turns a gap the recorder would have to detect into one
// it is told about in advance.
type Frame struct {
	Quotes   []Quote
	Shutdown bool
}

// DecodeFrame reads one raw push.
//
// The stream sends an array for !miniTicker@arr and a bare object for control events, so
// the shape is inspected before it is parsed. Guessing wrong here is not a crash — it is an
// empty frame, which reads exactly like a quiet market.
func DecodeFrame(raw []byte) (Frame, error) {
	var probe json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Frame{}, fmt.Errorf("marketdata: frame is not json: %w", err)
	}
	for _, b := range probe {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '[':
			return decodeQuotes(probe)
		case '{':
			return decodeControl(probe)
		}
		break
	}
	return Frame{}, errors.New("marketdata: frame is neither an array of tickers nor an event")
}

func decodeQuotes(raw json.RawMessage) (Frame, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return Frame{}, fmt.Errorf("marketdata: ticker array: %w", err)
	}

	out := Frame{Quotes: make([]Quote, 0, len(rows))}
	for i, row := range rows {
		q, err := quoteOf(row)
		if err != nil {
			return Frame{}, fmt.Errorf("marketdata: ticker %d: %w", i, err)
		}
		out.Quotes = append(out.Quotes, q)
	}
	return out, nil
}

func decodeControl(raw json.RawMessage) (Frame, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Frame{}, fmt.Errorf("marketdata: event: %w", err)
	}
	eventType, err := stringAt(fields, "e")
	if err != nil {
		return Frame{}, err
	}
	if eventType == eventServerShutdown {
		return Frame{Shutdown: true}, nil
	}
	return Frame{}, fmt.Errorf("%w: %q", ErrUnknownEvent, eventType)
}

func quoteOf(row map[string]json.RawMessage) (Quote, error) {
	eventType, err := stringAt(row, "e")
	if err != nil {
		return Quote{}, err
	}
	if eventType != eventMiniTicker {
		return Quote{}, fmt.Errorf("%w: %q", ErrUnknownEvent, eventType)
	}

	symbol, err := stringAt(row, "s")
	if err != nil {
		return Quote{}, err
	}

	// The close price. Read as a string and parsed with decimal, never through float64:
	// a price routed through a float comes back with different digits, and every number
	// derived from it is then wrong in a way no test on the derived number can see (L1).
	price, err := stringAt(row, "c")
	if err != nil {
		return Quote{}, fmt.Errorf("%s: %w", symbol, err)
	}
	amount, err := decimal.NewFromString(price)
	if err != nil {
		return Quote{}, fmt.Errorf("%s: price %q is not a decimal: %w", symbol, price, err)
	}

	millis, err := int64At(row, "E")
	if err != nil {
		return Quote{}, fmt.Errorf("%s: %w", symbol, err)
	}

	return Quote{
		Symbol: symbol,
		Price:  amount,
		// The venue's own timestamp, in UTC. Never our clock (K2): the age of a price is
		// how old the venue says it is, not how recently we happened to read the socket.
		ObservedAt: time.UnixMilli(millis).UTC(),
	}, nil
}

// stringAt reads a field by exact key.
//
// A tagged struct would not do, and this is the third place in the codebase that has had to
// say so. Every payload carries both "e" (event type, a string) and "E" (event time, a
// number); encoding/json falls back to case-insensitive matching and processes keys in
// document order, so a field tagged `json:"e"` is handed the string and then the number,
// and the number wins because it came second. The normalizer learned this, the stream
// ingester learned it again, and the trap is invisible at the call site — which is why it
// is written down every time rather than remembered.
func stringAt(fields map[string]json.RawMessage, key string) (string, error) {
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("marketdata: payload has no %q field", key)
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("marketdata: %q is not a string: %w", key, err)
	}
	return out, nil
}

func int64At(fields map[string]json.RawMessage, key string) (int64, error) {
	raw, ok := fields[key]
	if !ok {
		return 0, fmt.Errorf("marketdata: payload has no %q field", key)
	}
	var out int64
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("marketdata: %q is not a number: %w", key, err)
	}
	return out, nil
}
