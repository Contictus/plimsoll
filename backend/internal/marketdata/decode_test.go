package marketdata_test

import (
	"encoding/json"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/marketdata"
	"github.com/stretchr/testify/require"
)

// The shape !miniTicker@arr actually pushes, field order included. Order matters here: see
// TestTheEventTypeIsReadByExactKey.
const miniTickerArray = `[
  {"e":"24hrMiniTicker","E":1789041420000,"s":"BTCUSDT","c":"60123.45000000",
   "o":"59000.00000000","h":"61000.00000000","l":"58000.00000000","v":"100","q":"6000000"},
  {"e":"24hrMiniTicker","E":1789041420500,"s":"ETHUSDT","c":"2500.10000000",
   "o":"2400.00000000","h":"2600.00000000","l":"2300.00000000","v":"900","q":"2250000"}
]`

func TestAMiniTickerArrayDecodesToQuotes(t *testing.T) {
	frame, err := marketdata.DecodeFrame([]byte(miniTickerArray))
	require.NoError(t, err)
	require.False(t, frame.Shutdown)
	require.Len(t, frame.Quotes, 2)

	require.Equal(t, "BTCUSDT", frame.Quotes[0].Symbol)
	require.Equal(t, "60123.45", frame.Quotes[0].Price.String())
	require.Equal(t, int64(1789041420000), frame.Quotes[0].ObservedAt.UnixMilli(),
		"the venue's own event time, never ours (K2)")

	require.Equal(t, "ETHUSDT", frame.Quotes[1].Symbol)
	require.Equal(t, "2500.1", frame.Quotes[1].Price.String())
}

// THE TRAP, FOR THE THIRD TIME IN THIS CODEBASE.
//
// Every payload carries both "e" (event type, a string) and "E" (event time, a number).
// encoding/json falls back to case-insensitive matching, and it processes keys in document
// order — so a struct tagged `json:"e"` is handed the string, then handed the number, and
// the number wins because it came second. The normalizer learned this, the stream ingester
// learned it again, and this decoder reads by exact key from map[string]json.RawMessage
// for the same reason.
//
// This test fails against a tagged struct and passes against an exact-key read, which is
// the only difference between them that a payload can show.
func TestTheEventTypeIsReadByExactKey(t *testing.T) {
	// A tagged struct, written the way it would be by someone who had not been bitten.
	var naive []struct {
		EventType string `json:"e"`
	}
	naiveErr := json.Unmarshal([]byte(miniTickerArray), &naive)
	require.Error(t, naiveErr,
		"if this stops failing, encoding/json changed and the comment above needs rewriting")

	frame, err := marketdata.DecodeFrame([]byte(miniTickerArray))
	require.NoError(t, err, "the exact-key read must survive what the tagged struct cannot")
	require.Len(t, frame.Quotes, 2)
}

// F8: the market stream announces its own shutdown, which the user-data stream has no
// equivalent of. Recognising it turns a gap the recorder would have to detect into one it
// is told about in advance.
func TestServerShutdownIsRecognisedRatherThanRefused(t *testing.T) {
	frame, err := marketdata.DecodeFrame([]byte(`{"e":"serverShutdown","E":1789041420000}`))
	require.NoError(t, err)
	require.True(t, frame.Shutdown)
	require.Empty(t, frame.Quotes)
}

// L1, at the edge where it is easiest to lose. Binance sends prices as strings; a payload
// where one arrived as a JSON number is either a different venue or a proxy that reparsed
// it, and either way the digits can no longer be trusted.
func TestAPriceThatIsNotAStringIsRefused(t *testing.T) {
	_, err := marketdata.DecodeFrame([]byte(
		`[{"e":"24hrMiniTicker","E":1789041420000,"s":"BTCUSDT","c":60123.45}]`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "BTCUSDT")
}

// An event type this decoder has no rule for is refused, not skipped. Skipping is how a new
// kind of push becomes silence, and silence is the worst possible failure (L11).
func TestAnUnknownEventTypeIsRefused(t *testing.T) {
	_, err := marketdata.DecodeFrame([]byte(`{"e":"somethingNew","E":1789041420000}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "somethingNew")
}

func TestGarbageIsAnErrorAndNotAnEmptyFrame(t *testing.T) {
	_, err := marketdata.DecodeFrame([]byte(`not json`))
	require.Error(t, err)
}
