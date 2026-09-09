package marketdata_test

import (
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/marketdata"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// price_ticks is keyed by (instrument_id, ts), so the minute is the deduplication (K7).
// Two processes in different zones must produce the same key for the same instant, or the
// same minute is stored twice and "the last price in that minute" stops meaning anything.
func TestTheMinuteIsTruncatedInUTCWhateverTheInputZone(t *testing.T) {
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	require.NoError(t, err)

	instant := at("2026-09-09T14:37:59.999Z")
	want := at("2026-09-09T14:37:00Z")

	require.Equal(t, want, marketdata.Minute(instant))
	require.Equal(t, want, marketdata.Minute(instant.In(istanbul)),
		"the same instant in another zone must truncate to the same minute")
	require.Equal(t, time.UTC, marketdata.Minute(instant.In(istanbul)).Location(),
		"the stored timestamp must be UTC, not merely equal to it")
}

// F7: "only tickers that have changed will be present in the array". A symbol missing from
// a push has not lost its price -- it has not traded in that second. A Book that forgot it
// would blank the valuation of every thinly traded asset one second at a time.
func TestASymbolMissingFromAPushKeepsItsLastPrice(t *testing.T) {
	book := marketdata.NewBook()

	book.Observe(marketdata.Quote{
		Symbol: "BTCUSDT", Price: decimal.RequireFromString("60000"),
		ObservedAt: at("2026-09-09T14:00:00Z"),
	})
	book.Observe(marketdata.Quote{
		Symbol: "ETHUSDT", Price: decimal.RequireFromString("2500"),
		ObservedAt: at("2026-09-09T14:00:00Z"),
	})

	// The next push names only BTCUSDT. ETHUSDT simply did not trade.
	book.Observe(marketdata.Quote{
		Symbol: "BTCUSDT", Price: decimal.RequireFromString("60100"),
		ObservedAt: at("2026-09-09T14:00:01Z"),
	})

	btc, ok := book.Latest("BTCUSDT")
	require.True(t, ok)
	require.Equal(t, "60100", btc.Price.String())

	eth, ok := book.Latest("ETHUSDT")
	require.True(t, ok, "a symbol absent from a push must keep its last price")
	require.Equal(t, "2500", eth.Price.String())
	require.Equal(t, at("2026-09-09T14:00:00Z"), eth.ObservedAt,
		"and it must keep the age of that price, not inherit the age of the push")
}

// price_stale (L11) has to be computed from when the venue said the price, not from when we
// last looked at the socket. A book that refreshed every age on every push would report a
// permanently fresh price for a symbol that stopped trading a week ago.
func TestTheAgeBelongsToThePriceAndNotToTheConnection(t *testing.T) {
	book := marketdata.NewBook()
	stale := at("2026-09-09T09:00:00Z")
	fresh := at("2026-09-09T14:00:00Z")

	book.Observe(marketdata.Quote{Symbol: "OLDUSDT", Price: decimal.RequireFromString("1"), ObservedAt: stale})
	book.Observe(marketdata.Quote{Symbol: "NEWUSDT", Price: decimal.RequireFromString("2"), ObservedAt: fresh})

	require.Equal(t, stale, book.OldestObservedAt(),
		"the book's age is its worst price, never its best")
}

// An empty book has no age to report, and reporting the zero time would make every fresh
// system look infinitely stale.
func TestAnEmptyBookReportsNoAge(t *testing.T) {
	_, ok := marketdata.NewBook().Latest("BTCUSDT")
	require.False(t, ok)
	require.True(t, marketdata.NewBook().OldestObservedAt().IsZero())
}

// A price that arrives out of order -- a delayed frame after a newer one -- must not
// overwrite the newer price. The stream is not ordered by anything we control.
func TestAnOlderQuoteDoesNotOverwriteANewerOne(t *testing.T) {
	book := marketdata.NewBook()

	book.Observe(marketdata.Quote{
		Symbol: "BTCUSDT", Price: decimal.RequireFromString("60100"),
		ObservedAt: at("2026-09-09T14:00:01Z"),
	})
	book.Observe(marketdata.Quote{
		Symbol: "BTCUSDT", Price: decimal.RequireFromString("60000"),
		ObservedAt: at("2026-09-09T14:00:00Z"),
	})

	got, ok := book.Latest("BTCUSDT")
	require.True(t, ok)
	require.Equal(t, "60100", got.Price.String(),
		"a late frame carrying an older observation must not rewind the price")
}
