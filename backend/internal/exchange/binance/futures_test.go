package binance_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
