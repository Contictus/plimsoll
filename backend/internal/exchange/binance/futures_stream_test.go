package binance_test

import (
	"encoding/json"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/stretchr/testify/require"
)

// F17, asserted as a constant rather than trusted as a comment.
//
// The failure mode of getting this wrong is not an error. The legacy URL was decommissioned
// on 2026-04-23; a stream pointed at it connects SUCCESSFULLY and receives nothing, which
// looks exactly like an account with no activity. Nobody investigates a quiet account.
func TestTheFuturesStreamUsesTheMigratedURL(t *testing.T) {
	require.Equal(t, "wss://fstream.binance.com/private", binance.FuturesStreamURL)
	require.NotContains(t, binance.FuturesStreamURL, "/ws")
	require.NotContains(t, binance.FuturesStreamURL, "/stream")
}

// The stream says WHICH symbol moved. It deliberately does not mint an identity for the
// fill: that has to match what the REST walk produces (L5), and a second spelling of a key
// is a doubled position. So the trigger reads two fields and the walk reads the rest.
func TestAFillEventNamesItsSymbol(t *testing.T) {
	frame := json.RawMessage(`{"e":"ORDER_TRADE_UPDATE","E":1789000000000,
		"o":{"s":"BTCUSDT","S":"BUY","x":"TRADE","X":"PARTIALLY_FILLED"}}`)

	symbol, ok := binance.FuturesEventSymbol(frame)
	require.True(t, ok)
	require.Equal(t, "BTCUSDT", symbol)
}

// Most frames on this stream are not fills. Ignoring them is normal and must not look like
// an error: treating an order acknowledgement as a failure would turn an ordinary minute
// into a stopped supervisor.
func TestANonFillEventIsIgnoredRatherThanRefused(t *testing.T) {
	for _, frame := range []string{
		`{"e":"ACCOUNT_UPDATE","E":1789000000000,"a":{"B":[]}}`,
		`{"e":"listenKeyExpired","E":1789000000000}`,
		`{"e":"ORDER_TRADE_UPDATE","o":{}}`,
		`not json at all`,
	} {
		symbol, ok := binance.FuturesEventSymbol(json.RawMessage(frame))
		require.False(t, ok, "frame %q was read as a fill", frame)
		require.Empty(t, symbol)
	}
}

// A stream needs a client to mint its listenKey. Refusing at construction is what keeps the
// failure at startup rather than at the first fill.
func TestAFuturesStreamRefusesToBeBuiltWithoutAClient(t *testing.T) {
	_, err := binance.NewFuturesStream(binance.FuturesStreamConfig{})
	require.Error(t, err)
}
