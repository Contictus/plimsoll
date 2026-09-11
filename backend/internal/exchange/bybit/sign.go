package bybit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"time"
)

// sign builds the V5 signature for a GET.
//
// The payload is `timestamp + api_key + recv_window + queryString`, HMAC-SHA256, lower-case
// hex (B1). This is deliberately NOT Binance's scheme, and the difference is the kind that
// produces a working signature for the wrong string: Binance signs the query alone and sends
// the result as a parameter, Bybit signs a concatenation beginning with the timestamp and the
// key and sends it in a header. A client written by analogy authenticates nothing.
//
// The query string signed must be byte-identical to the one sent, which is why the caller
// passes url.Values and this function -- not the caller -- decides the encoding.
func sign(secret, apiKey string, at time.Time, recvWindow time.Duration, query url.Values) (
	timestamp, recv, signature, encodedQuery string,
) {
	timestamp = strconv.FormatInt(at.UnixMilli(), 10)
	recv = strconv.FormatInt(recvWindow.Milliseconds(), 10)
	encodedQuery = query.Encode()

	mac := hmac.New(sha256.New, []byte(secret))
	// Written as four appends rather than one concatenation so the order is visible: it is
	// the whole of the scheme, and a transposition here fails only against the live venue.
	mac.Write([]byte(timestamp))
	mac.Write([]byte(apiKey))
	mac.Write([]byte(recv))
	mac.Write([]byte(encodedQuery))

	return timestamp, recv, hex.EncodeToString(mac.Sum(nil)), encodedQuery
}
