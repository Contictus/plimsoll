package binance_test

import (
	"encoding/json"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/stretchr/testify/require"
)

// Discovery has to name every symbol before it can probe one (F4), and exchangeInfo is the
// only list there is. Every entry counts, whatever its status: a halted or broken symbol
// can still hold the fills that acquired an asset, and skipping it is the hole K26 refuses.
func TestSpotSymbolsListsEverySymbolWhateverItsStatus(t *testing.T) {
	raw := json.RawMessage(`{"symbols":[
		{"symbol":"ETHBTC","status":"TRADING"},
		{"symbol":"BTCUSDT","status":"TRADING"},
		{"symbol":"OLDCOINBTC","status":"BREAK"},
		{"symbol":"BTCUSDT","status":"TRADING"}
	]}`)

	got, err := binance.SpotSymbols(raw)
	require.NoError(t, err)
	require.Equal(t, []string{"BTCUSDT", "ETHBTC", "OLDCOINBTC"}, got,
		"sorted and deduplicated, so the discovery cursor means the same thing on every run")
}

// A symbols array that is present but empty is not an error -- it is an exchange with
// nothing listed. A payload that is not exchangeInfo at all is.
func TestSpotSymbolsRefusesAPayloadThatIsNotExchangeInfo(t *testing.T) {
	_, err := binance.SpotSymbols(json.RawMessage(`[]`))
	require.Error(t, err)

	empty, err := binance.SpotSymbols(json.RawMessage(`{"symbols":[]}`))
	require.NoError(t, err)
	require.Empty(t, empty)
}

// A nameless entry is refused rather than skipped: an empty symbol would be probed as an
// empty string, and a sweep that silently drops entries reports a complete discovery over
// an incomplete list.
func TestSpotSymbolsRefusesANamelessEntry(t *testing.T) {
	_, err := binance.SpotSymbols(json.RawMessage(`{"symbols":[{"status":"TRADING"}]}`))
	require.Error(t, err)
}
