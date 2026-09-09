// Package marketdata records what things cost, so valuation has something to work from.
//
// It is the one exchange-facing package that holds no credential and signs nothing. Prices
// are the same for every account, so this feed belongs to the process rather than to an
// integration — and the endpoint it connects to is documented as carrying market data only,
// which makes "this connection cannot see account data" a property of the transport rather
// than a promise the code keeps (docs/BINANCE-API-NOTES.md F6).
//
// Everything in this file is pure (L4): no clock, no network, no database. What it exists
// to get right is F7 — the all-market stream omits symbols that did not change, so a feed
// read naively reports a price as missing when it is merely unchanged.
package marketdata

import (
	"time"

	"github.com/shopspring/decimal"
)

// Quote is one price as the venue reported it.
//
// ObservedAt is the venue's own timestamp, never ours. price_stale (L11) is the age of a
// price, and measuring it from when we read the socket would report a permanently fresh
// price for a symbol that stopped trading a week ago.
type Quote struct {
	Symbol     string
	Price      decimal.Decimal
	ObservedAt time.Time
}

// Minute truncates an instant to the minute it falls in, in UTC.
//
// UTC, not local: price_ticks is keyed by (instrument_id, ts), so two processes in
// different zones must produce the same key for the same instant. Otherwise one minute is
// stored twice and "the last price in that minute" stops meaning anything (K7).
func Minute(t time.Time) time.Time {
	return t.UTC().Truncate(time.Minute)
}

// Book is the last known price per symbol.
//
// It exists because of F7: the all-market stream carries "only tickers that have changed",
// so a symbol absent from a push has not lost its price — it has not traded in that second.
// A reader that treated absence as absence would blank the valuation of every thinly traded
// asset an account holds, one second at a time, and the total would flicker for a reason no
// lineage could explain.
//
// Not safe for concurrent use. The recorder owns one and reads the socket on one goroutine;
// sharing it would be a second design decision, and it should be made deliberately rather
// than inherited from a mutex nobody asked for.
type Book struct {
	latest map[string]Quote
}

// NewBook returns an empty book. An empty book reports no age at all rather than the zero
// time, so a system that has just started does not look infinitely stale.
func NewBook() *Book {
	return &Book{latest: map[string]Quote{}}
}

// Observe records a quote, unless an equally recent or newer one is already held.
//
// The guard is not defensive tidiness. Frames arrive in whatever order the network
// delivers them, and a delayed push carrying an older observation would otherwise rewind
// the price — a movement the venue never made, in a number someone is about to act on.
func (b *Book) Observe(q Quote) {
	if held, ok := b.latest[q.Symbol]; ok && !q.ObservedAt.After(held.ObservedAt) {
		return
	}
	b.latest[q.Symbol] = q
}

// Latest returns the last known quote for a symbol. The second return distinguishes "we
// have never seen this symbol" from "its price is zero", which are different claims and
// only one of them is renderable.
func (b *Book) Latest(symbol string) (Quote, bool) {
	q, ok := b.latest[symbol]
	return q, ok
}

// Len is how many symbols the book holds.
func (b *Book) Len() int { return len(b.latest) }

// OldestObservedAt is the age of the worst price in the book, not the average.
//
// A portfolio priced through a BTC rate from one second ago and a USDC rate from an hour
// ago is an hour-old number. Averaging the two would report the most important weakness in
// the answer as a rounding difference.
//
// The zero time means the book is empty, which is why callers check IsZero rather than
// comparing against a tolerance.
func (b *Book) OldestObservedAt() time.Time {
	var oldest time.Time
	for _, q := range b.latest {
		if oldest.IsZero() || q.ObservedAt.Before(oldest) {
			oldest = q.ObservedAt
		}
	}
	return oldest
}
