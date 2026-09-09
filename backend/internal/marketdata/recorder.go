package marketdata

import (
	"context"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/store"
)

// SourceStream and SourceSnapshot name which feed filed a price. The valuation run records
// it per leg (K11): "one run per response" is only meaningful if the run can say where each
// number came from.
const (
	SourceStream   = "binance:miniTicker"
	SourceSnapshot = "binance:ticker"
)

// Symbols maps an exchange symbol to the instrument it means.
//
// Loaded once per recorder start rather than resolved per quote. The feed carries every
// symbol the venue lists -- thousands -- and resolving each one per second would be a
// database round trip per push. It is a snapshot of the registry, which is reference data
// that changes when an operator curates it, not while a stream is running.
type Symbols map[string]int64

// LoadSymbols reads the aliases in force at an instant.
//
// `at` is passed rather than assumed to be now, because the rule is the same rule the
// ledger follows: a symbol is resolved as of the time of the thing being resolved, never
// with today's mapping (L8, K22). For a live feed that instant is now; for a historical
// backfill it is the window being filled.
func LoadSymbols(ctx context.Context, db store.DBTX, exchange string, market instrument.Market, at time.Time) (Symbols, error) {
	rows, err := store.New(db).ListInstrumentAliasesAt(ctx, store.ListInstrumentAliasesAtParams{
		Exchange: exchange,
		Market:   string(market),
		At:       at,
	})
	if err != nil {
		return nil, fmt.Errorf("marketdata: read %s %s aliases: %w", exchange, market, err)
	}
	out := make(Symbols, len(rows))
	for _, r := range rows {
		out[r.ExchangeSymbol] = r.InstrumentID
	}
	return out, nil
}

// Recorder keeps price_ticks current from the public feed.
//
// It is a process-level component, not a per-integration one. Prices are the same for every
// account, and a feed per account would multiply an IP-wide budget by the number of users
// (K24).
type Recorder struct {
	Client  *Client
	Stream  *Stream
	Writer  Writer
	Symbols Symbols

	// Book is the last known price per symbol. Exported so the valuation run can read it
	// without a database round trip, and so its age is the age of the prices rather than
	// the age of the connection (F7).
	Book *Book
}

// NewRecorder assembles one. The book starts empty and is filled by Snapshot.
func NewRecorder(client *Client, stream *Stream, writer Writer, symbols Symbols) *Recorder {
	return &Recorder{Client: client, Stream: stream, Writer: writer, Symbols: symbols, Book: NewBook()}
}

// Snapshot fills the book and price_ticks from a single REST call.
//
// Mandatory rather than an optimisation, and F7 is why: the all-market stream carries "only
// tickers that have changed", so until a symbol trades the feed has nothing to say about
// it. Without a snapshot, a thinly traded holding has no price at all and the portfolio
// silently omits it.
func (r *Recorder) Snapshot(ctx context.Context) (int, error) {
	quotes, err := r.Client.Prices(ctx)
	if err != nil {
		return 0, err
	}
	return r.absorb(ctx, quotes, SourceSnapshot)
}

// Run keeps the book and the table current until ctx is cancelled or the feed stops.
func (r *Recorder) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-r.Stream.Frames():
			if !ok {
				return nil
			}
			if _, err := r.absorb(ctx, frame.Quotes, SourceStream); err != nil {
				return err
			}
		}
	}
}

// absorb records the quotes for symbols the registry knows, and reports how many landed.
//
// A symbol with no instrument is skipped in silence, and that silence is correct here
// rather than a violation of L11: the venue lists thousands of pairs and an account holds a
// handful. A warning per unknown symbol per second would be noise that buries the one
// reason a reader needs. What L11 governs is a number we serve, and an unlisted symbol
// contributes to none.
func (r *Recorder) absorb(ctx context.Context, quotes []Quote, source string) (int, error) {
	ticks := make([]Tick, 0, len(quotes))
	for _, q := range quotes {
		instrumentID, known := r.Symbols[q.Symbol]
		if !known {
			continue
		}
		r.Book.Observe(q)
		ticks = append(ticks, TickOf(instrumentID, q, source))
	}
	if len(ticks) == 0 {
		return 0, nil
	}
	if err := r.Writer.Record(ctx, ticks); err != nil {
		return 0, err
	}
	return len(ticks), nil
}
