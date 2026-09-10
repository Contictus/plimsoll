package binance_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/stretchr/testify/require"
)

func futuresExchangeInfo(t *testing.T) json.RawMessage {
	t.Helper()
	return loadFixture(t, "exchange_info_usdm.json", "payload")
}

// The futures universe is not the spot universe with a different base URL. Its
// exchangeInfo carries a contractType, and only one of the four values is a perpetual this
// project models -- so the sweep filters rather than takes the list.
//
// Pointing the spot sweep at this endpoint would open a walk for every quarterly future,
// each one a contract that DELIVERS. A delivered contract closes itself on a date; folded
// as a perp it stays open forever, and the position it leaves behind is a phantom exposure
// the user cannot close because it does not exist.
func TestOnlyPerpetualsThatAreTradingAreWalked(t *testing.T) {
	contracts, err := binance.FuturesContracts(futuresExchangeInfo(t))
	require.NoError(t, err)

	symbols := make([]string, 0, len(contracts))
	for _, c := range contracts {
		symbols = append(symbols, c.Symbol)
	}
	require.Equal(t, []string{"BTCUSDT", "ETHUSDT"}, symbols)
}

// A USD-M perp settles in its margin asset, and that is the asset funding is paid in. An
// instrument that does not name it makes the funding fold guess -- and the guess is right
// for USDT-margined contracts and wrong for every other kind, which is the worst shape a
// bug can have: correct in testing, wrong in production.
func TestAPerpNamesTheAssetItSettlesIn(t *testing.T) {
	contracts, err := binance.FuturesContracts(futuresExchangeInfo(t))
	require.NoError(t, err)
	require.NotEmpty(t, contracts)

	require.Equal(t, "BTC", contracts[0].BaseAsset)
	require.Equal(t, "USDT", contracts[0].QuoteAsset)
	require.Equal(t, "USDT", contracts[0].MarginAsset)
}

// A quarterly future is not a perpetual, and neither is a perpetual over an index the asset
// registry has no coin for. Refused by name rather than by omission, so that a contract type
// Binance adds later stops the sweep instead of quietly joining it.
func TestAContractTypeOutsideTheModelledSetIsRefused(t *testing.T) {
	for _, contractType := range []string{"CURRENT_QUARTER", "NEXT_QUARTER", "TRADIFI_PERPETUAL", "INVENTED"} {
		require.False(t, binance.IsModelledContract(contractType), contractType)
	}
	require.True(t, binance.IsModelledContract("PERPETUAL"))
}

// A symbol with no name is a hole in the sweep, and a sweep over a list with holes reports a
// complete discovery it did not do -- the same refusal SpotSymbols makes, for the same
// reason (K33).
func TestAContractWithNoSymbolStopsTheSweep(t *testing.T) {
	_, err := binance.FuturesContracts(json.RawMessage(
		`{"symbols":[{"symbol":"","contractType":"PERPETUAL","status":"TRADING",` +
			`"baseAsset":"BTC","quoteAsset":"USDT","marginAsset":"USDT"}]}`))
	require.Error(t, err)
}

// A perpetual that names no margin asset cannot be stored: the schema requires a settle
// asset on a perp (00005), and inventing one is how a coin-margined contract gets valued as
// if it were USDT-margined -- a whole-position error, not a rounding one.
func TestAPerpWithNoMarginAssetStopsTheSweep(t *testing.T) {
	_, err := binance.FuturesContracts(json.RawMessage(
		`{"symbols":[{"symbol":"BTCUSDT","contractType":"PERPETUAL","status":"TRADING",` +
			`"baseAsset":"BTC","quoteAsset":"USDT","marginAsset":""}]}`))
	require.Error(t, err)
}

// Spot and futures are separate services, and a futures path on the spot host is a 404 --
// which surfaces as "no such endpoint" rather than as "wrong host", and sends a reader
// looking for a typo in a path that is correct. The host is chosen once, here, and this is
// the test that says so.
func TestAFuturesRequestGoesToTheFuturesHost(t *testing.T) {
	var asked string
	futures := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		_, _ = w.Write([]byte(`{"symbols":[]}`))
	}))
	defer futures.Close()

	spot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a futures request reached the spot host")
		_, _ = w.Write([]byte(`{"symbols":[]}`))
	}))
	defer spot.Close()

	client, _, _ := newClientWithFutures(t, spot, futures)
	_, err := client.FuturesExchangeInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/fapi/v1/exchangeInfo", asked)
}

const usdmBTCUSDT int64 = 501

// futuresTrades returns the fixture's rows: [0] a buy, [1] a sell that realized PnL,
// [2] a hedge-mode row.
func futuresTrades(t *testing.T) []json.RawMessage {
	t.Helper()
	var rows []json.RawMessage
	require.NoError(t, json.Unmarshal(loadFixture(t, "user_trades_usdm.json", "payload"), &rows))
	require.Len(t, rows, 3)
	return rows
}

func usdmResolver(t *testing.T) *fakeResolver {
	t.Helper()
	r := assetResolverFor("USDT", usdtAsset)
	r.windows = []aliasWindow{{
		symbol:     "BTCUSDT",
		from:       time.Unix(0, 0),
		to:         time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		instrument: usdmBTCUSDT,
	}}
	return r
}

func normalizeFuturesTrade(t *testing.T, raw json.RawMessage) (ledger.Event, error) {
	t.Helper()
	tc := testContext()
	tc.Source = binance.SourceREST
	return binance.NormalizeFuturesTrade(context.Background(), usdmResolver(t), tc, raw)
}

// One userTrades row is one TRADE, under the usdm market so it can never collide with the
// spot fill of the same ticker (K10, L8).
func TestAFuturesFillMapsEveryField(t *testing.T) {
	raw := futuresTrades(t)[0]

	event, err := normalizeFuturesTrade(t, raw)
	require.NoError(t, err)

	require.Equal(t, ledger.TypeTrade, event.EventType)
	require.Equal(t, "usdm:trade:BTCUSDT:8886774", event.VenueEventID)
	require.Equal(t, int64(8886774), event.VenueSequence)
	require.NotNil(t, event.InstrumentID)
	require.Equal(t, usdmBTCUSDT, *event.InstrumentID)
	require.Equal(t, ledger.SideBuy, event.Side)
	require.True(t, event.Quantity.Decimal.Equal(decimal.RequireFromString("0.500")))
	require.True(t, event.Price.Decimal.Equal(decimal.RequireFromString("60000.00")))
	require.Equal(t, time.UnixMilli(1757400000000).UTC(), event.EventTime.UTC())

	// The commission rides on the fill that caused it and is never folded into the price
	// (K18, L9).
	require.True(t, event.Fee.Decimal.Equal(decimal.RequireFromString("12.00")))
	require.Equal(t, "USDT", event.FeeAsset)
	require.NotNil(t, event.FeeAssetID)

	require.Equal(t, []byte(raw), []byte(event.Raw))
}

// The instrument is looked up in the USD-M market, not in whatever market the caller
// happened to pass. Spot BTCUSDT and perp BTCUSDT are the same string and different
// instruments; resolving a perp fill against the spot alias attaches a leveraged position's
// quantity to a spot one, and both numbers are then wrong while looking plausible.
func TestAFuturesFillResolvesInTheFuturesMarket(t *testing.T) {
	r := usdmResolver(t)
	tc := testContext()

	_, err := binance.NormalizeFuturesTrade(context.Background(), r, tc, futuresTrades(t)[0])
	require.NoError(t, err)

	require.NotEmpty(t, r.markets)
	require.Equal(t, instrument.MarketUSDM, r.markets[0],
		"a perp fill was resolved in the wrong market")
}

// K5 AND L3, in one assertion.
//
// The venue sends realizedPnl on the fill and the position engine computes realized PnL
// itself from the average-cost fold. Storing the venue's copy would put two numbers for one
// fact in the system, and the fold would then either disagree with it -- a finding nobody
// asked for -- or be replaced by it, which is the second source of truth L3 forbids.
//
// It survives in raw forever (L15), which is what makes it a reconciliation input for M7
// rather than something thrown away.
func TestTheVenuesRealizedPnLIsNotStoredOnTheEvent(t *testing.T) {
	event, err := normalizeFuturesTrade(t, futuresTrades(t)[1])
	require.NoError(t, err)

	require.Equal(t, ledger.SideSell, event.Side)
	require.True(t, event.Price.Decimal.Equal(decimal.RequireFromString("62000.00")))

	// There is no column it could have gone into, so the assertion is on the payload: the
	// fixture's row carries 500.00 and the event carries no 500 anywhere.
	require.NotContains(t, event.Quantity.Decimal.String(), "500.00")
	require.Contains(t, string(event.Raw), `"realizedPnl": "500.00"`,
		"the venue's number must survive in raw, or M7 has nothing to reconcile against")
}

// V1 is one-way mode (K5). A hedge-mode account reports LONG and SHORT rows for one symbol,
// and this fold has one position per instrument -- so folding both sides into it averages a
// long and a short together and reports a position that is flat when the account is carrying
// two live exposures.
//
// Refused loudly rather than skipped: an account in the wrong mode must fail to import, not
// import halfway.
func TestAHedgeModeFillIsRefused(t *testing.T) {
	_, err := normalizeFuturesTrade(t, futuresTrades(t)[2])
	require.ErrorIs(t, err, binance.ErrHedgeMode)
}

// A row that cannot be identified, timed, priced or sized is refused rather than stored with
// a hole in it -- the same refusals the spot and transfer normalizers make.
func TestAnUnusableFuturesFillIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, row string }{
		{"no trade id", `{"symbol":"BTCUSDT","id":0,"side":"BUY","price":"1","qty":"1","positionSide":"BOTH","time":1757400000000}`},
		{"no symbol", `{"symbol":"","id":1,"side":"BUY","price":"1","qty":"1","positionSide":"BOTH","time":1757400000000}`},
		{"no time", `{"symbol":"BTCUSDT","id":1,"side":"BUY","price":"1","qty":"1","positionSide":"BOTH","time":0}`},
		{"no side", `{"symbol":"BTCUSDT","id":1,"side":"","price":"1","qty":"1","positionSide":"BOTH","time":1757400000000}`},
		{"zero quantity", `{"symbol":"BTCUSDT","id":1,"side":"BUY","price":"1","qty":"0","positionSide":"BOTH","time":1757400000000}`},
		{"zero price", `{"symbol":"BTCUSDT","id":1,"side":"BUY","price":"0","qty":"1","positionSide":"BOTH","time":1757400000000}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeFuturesTrade(t, json.RawMessage(tc.row))
			require.Error(t, err, "a futures fill with %s was normalized", tc.name)
		})
	}
}

// The side comes from `side`, not from `buyer`. They agree on this endpoint today, and a
// normalizer that read the boolean would be resting on that agreement -- a derived field
// standing in for the authoritative one, which is exactly the reading that breaks quietly
// when a venue changes what it derives.
func TestTheSideComesFromTheSideField(t *testing.T) {
	event, err := normalizeFuturesTrade(t, json.RawMessage(
		`{"symbol":"BTCUSDT","id":42,"side":"SELL","price":"1","qty":"1","buyer":true,`+
			`"positionSide":"BOTH","time":1757400000000}`))
	require.NoError(t, err)
	require.Equal(t, ledger.SideSell, event.Side,
		"the authoritative side field lost to a derived boolean")
}
