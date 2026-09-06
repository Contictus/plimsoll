package binance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/asset"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

const bnbbtcInstrument int64 = 4242

// fakeResolver stands in for the alias table. It records what it was asked, because the
// question this package has to get right is not "which id came back" but "which instant
// was the lookup made at" (L8).
type fakeResolver struct {
	// windows maps a symbol to the instrument it resolved to during a half-open time
	// window, so a test can prove the resolver was called with the trade's own time.
	windows      []aliasWindow
	calls        []resolveCall
	assetWindows []aliasWindow
	assetCalls   []resolveCall
}

// Asset resolves a coin ticker the same way, and records the instant it was asked for.
func (f *fakeResolver) Asset(_ context.Context, symbol string, at time.Time) (int64, error) {
	f.assetCalls = append(f.assetCalls, resolveCall{symbol, at})
	for _, w := range f.assetWindows {
		if w.symbol == symbol && !at.Before(w.from) && at.Before(w.to) {
			return w.instrument, nil
		}
	}
	return 0, asset.ErrUnknownSymbol
}

// assetResolverFor is the common case: one coin, always resolvable.
func assetResolverFor(symbol string, id int64) *fakeResolver {
	return &fakeResolver{assetWindows: []aliasWindow{{
		symbol:     symbol,
		from:       time.Unix(0, 0),
		to:         time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		instrument: id,
	}}}
}

type aliasWindow struct {
	symbol     string
	from, to   time.Time
	instrument int64
}

type resolveCall struct {
	symbol string
	at     time.Time
}

func (f *fakeResolver) Instrument(
	_ context.Context, _ instrument.Market, symbol string, at time.Time,
) (int64, error) {
	f.calls = append(f.calls, resolveCall{symbol, at})
	for _, w := range f.windows {
		if w.symbol != symbol {
			continue
		}
		if !at.Before(w.from) && at.Before(w.to) {
			return w.instrument, nil
		}
	}
	return 0, instrument.ErrUnknownSymbol
}

// resolverFor is the common case: one symbol, always resolvable.
func resolverFor(symbol string, id int64) *fakeResolver {
	return &fakeResolver{windows: []aliasWindow{{
		symbol:     symbol,
		from:       time.Unix(0, 0),
		to:         time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		instrument: id,
	}}}
}

func testContext() binance.IngestContext {
	return binance.IngestContext{
		AccountID:     uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		IntegrationID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
	}
}

// loadFixture reads a fixture and returns the bytes at the given JSON path. The fixture's
// own underscore keys are provenance for a human and are never handed to the normalizer.
func loadFixture(t *testing.T, name string, path ...string) json.RawMessage {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "fixtures", "binance", name))
	require.NoError(t, err, "fixture %s", name)

	current := json.RawMessage(blob)
	for _, step := range path {
		var object map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(current, &object), "fixture %s at %q", name, step)
		next, ok := object[step]
		require.True(t, ok, "fixture %s has no key %q", name, step)
		current = next
	}
	return current
}

func firstTrade(t *testing.T) json.RawMessage {
	t.Helper()
	var trades []json.RawMessage
	require.NoError(t, json.Unmarshal(loadFixture(t, "mytrades_bnbbtc.json", "payload"), &trades))
	require.NotEmpty(t, trades)
	return trades[0]
}

func streamFill(t *testing.T) json.RawMessage {
	return loadFixture(t, "execution_report_trade_bnbbtc.json", "event")
}

// THE M2 EXIT CRITERION.
//
// One trade, seen down two paths. REST backfill and the live stream report it with
// different field names, a different envelope and a different "who saw it first", and they
// must produce the same identity and the same numbers. If they do not, the ledger's UNIQUE
// (integration_id, venue_event_id) inserts both and the position doubles -- silently,
// because both rows are individually correct (K19, L5).
func TestSameTradeFromRestAndStreamIsOneEvent(t *testing.T) {
	ctx := context.Background()
	tc := testContext()

	restCtx, streamCtx := tc, tc
	restCtx.Source = binance.SourceREST
	streamCtx.Source = binance.SourceStream

	fromREST, err := binance.NormalizeSpotTrade(
		ctx, resolverFor("BNBBTC", bnbbtcInstrument), restCtx, firstTrade(t))
	require.NoError(t, err)

	fromStream, err := binance.NormalizeStreamExecutionReport(
		ctx, resolverFor("BNBBTC", bnbbtcInstrument), streamCtx, streamFill(t))
	require.NoError(t, err)

	require.Equal(t, "spot:trade:BNBBTC:28457", fromREST.VenueEventID)
	require.Equal(t, fromREST.VenueEventID, fromStream.VenueEventID,
		"two paths, one identity: this is what makes ON CONFLICT DO NOTHING a correctness mechanism")

	require.Equal(t, fromREST.VenueSequence, fromStream.VenueSequence)
	require.Equal(t, fromREST.EventTime, fromStream.EventTime)
	require.Equal(t, fromREST.Side, fromStream.Side)
	require.True(t, fromREST.Quantity.Decimal.Equal(fromStream.Quantity.Decimal),
		"quantity: rest %s stream %s", fromREST.Quantity.Decimal, fromStream.Quantity.Decimal)
	require.True(t, fromREST.Price.Decimal.Equal(fromStream.Price.Decimal),
		"price: rest %s stream %s", fromREST.Price.Decimal, fromStream.Price.Decimal)
	require.True(t, fromREST.Fee.Decimal.Equal(fromStream.Fee.Decimal),
		"fee: rest %s stream %s", fromREST.Fee.Decimal, fromStream.Fee.Decimal)
	require.Equal(t, fromREST.FeeAsset, fromStream.FeeAsset)

	// The one thing that must differ. Source is metadata -- who saw it first -- and it is
	// excluded from identity on purpose.
	require.NotEqual(t, fromREST.Source, fromStream.Source)
}

func TestRestTradeMapsEveryField(t *testing.T) {
	tc := testContext()
	tc.Source = binance.SourceREST
	resolver := resolverFor("BNBBTC", bnbbtcInstrument)
	raw := firstTrade(t)

	event, err := binance.NormalizeSpotTrade(context.Background(), resolver, tc, raw)
	require.NoError(t, err)

	require.Equal(t, tc.AccountID, event.AccountID)
	require.Equal(t, tc.IntegrationID, event.IntegrationID)
	require.Equal(t, ledger.TypeTrade, event.EventType)
	require.Equal(t, ledger.SideBuy, event.Side)
	require.Equal(t, int64(28457), event.VenueSequence)
	require.NotNil(t, event.InstrumentID)
	require.Equal(t, bnbbtcInstrument, *event.InstrumentID)

	require.True(t, event.Quantity.Decimal.Equal(decimal.RequireFromString("12.00000000")),
		"quantity %s", event.Quantity.Decimal)
	require.Equal(t, "4.000001", event.Price.Decimal.String())
	require.Equal(t, "10.1", event.Fee.Decimal.String())
	require.Equal(t, "BNB", event.FeeAsset)

	// The exchange's own millisecond timestamp, in UTC. Never our clock (K2).
	require.Equal(t, time.UnixMilli(1499865549590).UTC(), event.EventTime.UTC())

	// L15: raw is the payload verbatim. Compared byte for byte, because "equivalent JSON"
	// is not the property -- being able to replay the exact bytes six months from now is.
	require.Equal(t, []byte(raw), []byte(event.Raw))
}

// isBuyer and S are the same fact spelled two ways. Getting either backwards inverts the
// position, and the number stays plausible the whole way down.
func TestSideMapsBothWaysOnBothPaths(t *testing.T) {
	tc := testContext()
	tc.Source = binance.SourceREST

	for _, tt := range []struct {
		isBuyer bool
		want    ledger.Side
	}{{true, ledger.SideBuy}, {false, ledger.SideSell}} {
		raw := mutateJSON(t, firstTrade(t), map[string]any{"isBuyer": tt.isBuyer})
		event, err := binance.NormalizeSpotTrade(
			context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, raw)
		require.NoError(t, err)
		require.Equal(t, tt.want, event.Side, "isBuyer=%v", tt.isBuyer)
	}

	for _, tt := range []struct {
		side string
		want ledger.Side
	}{{"BUY", ledger.SideBuy}, {"SELL", ledger.SideSell}} {
		raw := mutateJSON(t, streamFill(t), map[string]any{"S": tt.side})
		event, err := binance.NormalizeStreamExecutionReport(
			context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, raw)
		require.NoError(t, err)
		require.Equal(t, tt.want, event.Side, "S=%s", tt.side)
	}
}

func TestUnknownSideIsRefused(t *testing.T) {
	tc := testContext()
	raw := mutateJSON(t, streamFill(t), map[string]any{"S": "SIDEWAYS"})
	_, err := binance.NormalizeStreamExecutionReport(
		context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, raw)
	require.Error(t, err, "a side we cannot read must not default to buy")
}

// L1: 18 decimal places must survive. A quantity routed through float64 comes back with
// different digits, and every later number is checked against those digits.
func TestMoneyFieldsSurviveEighteenDecimalPlaces(t *testing.T) {
	const qty = "0.123456789012345678"
	const price = "12345678901234567890.123456789012345678"
	tc := testContext()
	tc.Source = binance.SourceREST

	raw := mutateJSON(t, firstTrade(t), map[string]any{
		"qty": qty, "price": price, "commission": qty,
	})
	event, err := binance.NormalizeSpotTrade(
		context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, raw)
	require.NoError(t, err)

	require.Equal(t, qty, event.Quantity.Decimal.String())
	require.Equal(t, price, event.Price.Decimal.String())
	require.Equal(t, qty, event.Fee.Decimal.String())
}

// L8: the symbol is resolved as of the event's own time, never as of now. Two windows for
// one symbol, and only the trade's timestamp picks the right one -- this is the industry's
// most common silent corruption, and the test has to be able to catch it.
func TestSymbolIsResolvedAtTheEventsOwnTime(t *testing.T) {
	const oldInstrument, recycledInstrument = int64(101), int64(202)
	tradeTime := time.UnixMilli(1499865549590).UTC()

	resolver := &fakeResolver{windows: []aliasWindow{
		{"BNBBTC", time.Unix(0, 0), tradeTime.Add(time.Hour), oldInstrument},
		{"BNBBTC", tradeTime.Add(time.Hour), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), recycledInstrument},
	}}

	tc := testContext()
	tc.Source = binance.SourceREST
	event, err := binance.NormalizeSpotTrade(context.Background(), resolver, tc, firstTrade(t))
	require.NoError(t, err)

	require.Equal(t, oldInstrument, *event.InstrumentID,
		"resolved with the wrong window: a 2017 trade must not get the 2026 mapping")
	require.Len(t, resolver.calls, 1)
	require.Equal(t, tradeTime, resolver.calls[0].at.UTC(),
		"the resolver must be called with the trade's time, not with time.Now()")
}

// An unresolvable symbol is an error carrying the symbol, so the caller can raise an
// unknown_symbol finding (K14). Dropping the trade instead would put a hole in the fold
// that shows up as a wrong average entry price and nothing else.
func TestUnresolvableSymbolIsAnErrorNamingTheSymbol(t *testing.T) {
	tc := testContext()
	_, err := binance.NormalizeSpotTrade(
		context.Background(), resolverFor("ETHBTC", 7), tc, firstTrade(t))
	require.ErrorIs(t, err, instrument.ErrUnknownSymbol)
	require.Contains(t, err.Error(), "BNBBTC")
}

// L9: the commission belongs to this event, on its own columns, in its own asset. Folding
// it into the price is the classic mistake -- it is invisible on one trade and wrong on
// every cost basis afterwards.
func TestFeeStaysOffThePrice(t *testing.T) {
	tc := testContext()
	tc.Source = binance.SourceREST

	event, err := binance.NormalizeSpotTrade(
		context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, firstTrade(t))
	require.NoError(t, err)

	// The payload's own price, untouched by the 10.1 BNB commission in any direction.
	require.True(t, event.Price.Decimal.Equal(decimal.RequireFromString("4.00000100")),
		"price was adjusted by the fee: got %s", event.Price.Decimal)
	require.True(t, event.Fee.Decimal.Equal(decimal.RequireFromString("10.10000000")))
	require.Equal(t, "BNB", event.FeeAsset)
}

// A fee of zero in an unnamed asset is what Binance sends when nothing was charged. The
// schema requires fee and fee_asset to be null together, so a zero fee with no asset must
// become two nulls rather than one of each.
func TestZeroFeeWithNoAssetBecomesNoFeeAtAll(t *testing.T) {
	tc := testContext()
	raw := mutateJSON(t, streamFill(t), map[string]any{"n": "0", "N": nil})

	event, err := binance.NormalizeStreamExecutionReport(
		context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, raw)
	require.NoError(t, err)
	require.False(t, event.Fee.Valid, "a fee with no asset cannot be stored")
	require.Empty(t, event.FeeAsset)
}

// An executionReport is mostly not a fill. A placement, a cancellation and an expiry all
// arrive on the same channel and none of them moves a position.
func TestNonFillExecutionReportsAreNotEvents(t *testing.T) {
	tc := testContext()

	t.Run("the documented NEW payload", func(t *testing.T) {
		raw := loadFixture(t, "execution_report_new.json", "event")
		_, err := binance.NormalizeStreamExecutionReport(
			context.Background(), resolverFor("ETHBTC", 9), tc, raw)
		require.ErrorIs(t, err, binance.ErrNotAFill)
	})

	for _, executionType := range []string{"NEW", "CANCELED", "REJECTED", "EXPIRED"} {
		t.Run(executionType, func(t *testing.T) {
			raw := mutateJSON(t, streamFill(t), map[string]any{"x": executionType, "t": -1})
			_, err := binance.NormalizeStreamExecutionReport(
				context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, raw)
			require.ErrorIs(t, err, binance.ErrNotAFill)
		})
	}
}

// An execution type we have never seen must stop the ingest loudly. Treating it as "not a
// fill" would silently drop a trade if Binance ever introduces a new way to report one --
// and a missing acquisition poisons the cost basis permanently (K26).
func TestUnknownExecutionTypeIsRefusedRatherThanSkipped(t *testing.T) {
	tc := testContext()
	raw := mutateJSON(t, streamFill(t), map[string]any{"x": "SETTLED_BY_ORACLE"})

	_, err := binance.NormalizeStreamExecutionReport(
		context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, raw)
	require.Error(t, err)
	require.NotErrorIs(t, err, binance.ErrNotAFill,
		"an unknown execution type is not a known non-fill; it must not be skipped")
	require.Contains(t, err.Error(), "SETTLED_BY_ORACLE")
}

// encoding/json matches an object key to a struct field case-insensitively when no exact
// match exists, and Binance's stream abbreviations differ only by case: `t` is the trade id
// and `T` the transaction time, `l` the last quantity and `L` the last price, `n` the
// commission and `N` its asset. One undeclared key is enough for a millisecond timestamp to
// be read as a trade id -- and the event that comes out looks entirely plausible.
//
// This test pins the exact-key reading. It fails loudly against a tagged struct that omits
// any of the paired keys.
func TestPairedKeysAreReadCaseSensitively(t *testing.T) {
	tc := testContext()
	tc.Source = binance.SourceStream

	event, err := binance.NormalizeStreamExecutionReport(
		context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, streamFill(t))
	require.NoError(t, err)

	// t=28457 and T=1499865549590 in the fixture. Reading either as the other is the bug.
	require.Equal(t, int64(28457), event.VenueSequence, "t (trade id) was read from T")
	require.Equal(t, time.UnixMilli(1499865549590).UTC(), event.EventTime.UTC(),
		"T (transaction time) was read from t")

	// l="12.00000000" and L="4.00000100": quantity and price, not the other way round.
	require.True(t, event.Quantity.Decimal.Equal(decimal.RequireFromString("12.00000000")),
		"l (last quantity) was read from L: got %s", event.Quantity.Decimal)
	require.True(t, event.Price.Decimal.Equal(decimal.RequireFromString("4.00000100")),
		"L (last price) was read from l: got %s", event.Price.Decimal)

	// n="10.10000000" and N="BNB": the amount and its asset.
	require.True(t, event.Fee.Decimal.Equal(decimal.RequireFromString("10.10000000")))
	require.Equal(t, "BNB", event.FeeAsset)

	// s="BNBBTC" and S="BUY". A symbol read from the side would not resolve at all.
	require.Equal(t, ledger.SideBuy, event.Side)
}

// A field Binance adds next year must not break ingestion. This is the opposite rule from
// the one above, and the distinction is the point: an unknown *field* is additive, an
// unknown *enum value* changes what the payload means.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	tc := testContext()
	tc.Source = binance.SourceREST
	raw := mutateJSON(t, firstTrade(t), map[string]any{
		"someFieldFromNextYear": "surprise",
		"anotherOne":            42,
	})

	_, err := binance.NormalizeSpotTrade(
		context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, raw)
	require.NoError(t, err)
}

// A fill with no trade id has no identity, and an event with no identity cannot be
// deduplicated. Writing it with a made-up id would double the position on the next replay.
func TestFillWithoutATradeIDIsRefused(t *testing.T) {
	tc := testContext()
	raw := mutateJSON(t, streamFill(t), map[string]any{"t": -1})

	_, err := binance.NormalizeStreamExecutionReport(
		context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, raw)
	require.Error(t, err)
	require.NotErrorIs(t, err, binance.ErrNotAFill,
		"x=TRADE with no trade id is a contradiction, not a skippable message")
}

// The schema refuses a fill with a zero or missing quantity, and finding that out at the
// INSERT means the whole batch fails. The normalizer says so first, naming the trade.
func TestFillWithoutAQuantityIsRefused(t *testing.T) {
	tc := testContext()
	tc.Source = binance.SourceREST

	for name, patch := range map[string]map[string]any{
		"zero quantity":    {"qty": "0"},
		"missing quantity": {"qty": ""},
		"negative price":   {"price": "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := mutateJSON(t, firstTrade(t), patch)
			_, err := binance.NormalizeSpotTrade(
				context.Background(), resolverFor("BNBBTC", bnbbtcInstrument), tc, raw)
			require.Error(t, err)
		})
	}
}

// mutateJSON applies patch to a payload and returns the result. Values are decoded with
// UseNumber so a patched fixture keeps every digit of the fields it did not touch (L1).
func mutateJSON(t *testing.T, raw json.RawMessage, patch map[string]any) json.RawMessage {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	require.NoError(t, decoder.Decode(&object), "payload is not an object")
	for key, value := range patch {
		object[key] = value
	}
	out, err := json.Marshal(object)
	require.NoError(t, err)
	return out
}
