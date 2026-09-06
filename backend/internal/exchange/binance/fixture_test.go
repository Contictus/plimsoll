package binance_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/stretchr/testify/require"
)

// Redaction happens in the write path, never as a later pass. "Record now, redact before
// committing" is how a real address reaches git history, where deleting it does not remove
// it.
func TestRedactReplacesIdentifyingFieldsAtEveryDepth(t *testing.T) {
	raw := json.RawMessage(`{
	  "address": "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	  "addressTag": "101672491",
	  "txId": "b3c6219639c8ae3f9cf010cdc24fw7f7yt8j1e0409b8b3b8ff8e79f4b4c4b",
	  "email": "trader@example.test",
	  "uid": "38176385",
	  "nested": {
	    "address": "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh",
	    "keep": "visible"
	  },
	  "list": [{"txKey": "abc"}, {"amount": "0.00123456"}]
	}`)

	got, err := binance.Redact(raw)
	require.NoError(t, err)

	text := string(got)
	for _, secret := range []string{
		"0xdeadbeef", "101672491", "b3c6219639c8", "trader@example.test", "38176385",
		"bc1qxy2kgdyg", `"abc"`,
	} {
		require.NotContains(t, text, secret, "redaction missed %q", secret)
	}
	require.Contains(t, text, "visible", "redaction must not remove non-identifying fields")
	require.Contains(t, text, "0.00123456", "amounts are the point of the fixture")
}

// Every field the redactor touches must still be present. Deleting the key instead of
// replacing the value would change the payload's shape, and a parser tested against that
// shape would not be tested against Binance's.
func TestRedactKeepsTheShape(t *testing.T) {
	got, err := binance.Redact(json.RawMessage(`{"address":"x","amount":"1"}`))
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(got, &decoded))
	require.Contains(t, decoded, "address")
	require.Equal(t, binance.RedactedPlaceholder, decoded["address"])
}

// L1: a fixture is a money fixture. Decoding a quantity through float64 and re-encoding it
// silently rewrites the digits, and the rewritten value is what every later test would be
// checked against.
func TestRedactPreservesNumericPrecisionExactly(t *testing.T) {
	const exact = `12345678901234567890.123456789012345678`
	raw := json.RawMessage(`{"qty":"` + exact + `","tradeId":` + exact + `}`)

	got, err := binance.Redact(raw)
	require.NoError(t, err)

	require.Contains(t, string(got), `"`+exact+`"`, "string amount was rewritten")
	require.Contains(t, string(got), exact, "bare number was rewritten")
}

func TestRedactHandlesATopLevelArray(t *testing.T) {
	got, err := binance.Redact(json.RawMessage(`[{"address":"x","qty":"1.5"}]`))
	require.NoError(t, err)

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal(got, &decoded))
	require.Len(t, decoded, 1)
	require.Equal(t, binance.RedactedPlaceholder, decoded[0]["address"])
}

func TestRedactRejectsBytesThatAreNotJSON(t *testing.T) {
	_, err := binance.Redact(json.RawMessage(`<html>gateway error</html>`))
	require.Error(t, err, "an unparsable body must not be written to a fixture unredacted")
}

// An object payload carries its provenance inline, the way every existing fixture does.
func TestFixtureAnnotatesAnObjectInline(t *testing.T) {
	got, err := binance.Fixture(json.RawMessage(`{"ok":true}`), "/sapi/v1/account/apiRestrictions")
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(got, &decoded))
	require.Equal(t, "recorded", decoded["_source"])
	require.Equal(t, "/sapi/v1/account/apiRestrictions", decoded["_endpoint"])
	require.Equal(t, true, decoded["ok"])
}

// An array has nowhere to put a _source key, so it is wrapped rather than mutated. The
// alternative -- annotating some fixtures and not others -- is the one that gets a file
// committed with no provenance at all.
func TestFixtureWrapsAnArrayRatherThanLosingProvenance(t *testing.T) {
	got, err := binance.Fixture(json.RawMessage(`[{"id":28457}]`), "/api/v3/myTrades")
	require.NoError(t, err)

	var decoded struct {
		Source  string          `json:"_source"`
		Payload json.RawMessage `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(got, &decoded))
	require.Equal(t, "recorded", decoded.Source)
	require.JSONEq(t, `[{"id":28457}]`, string(decoded.Payload))
}

// The last gate before bytes reach disk. Redaction works from a list of field names, and a
// list is a thing Binance can outgrow; this check does not care what the field is called.
func TestFixtureRefusesToWriteAnythingContainingASecret(t *testing.T) {
	const secret = "PLIMSOLL-TEST-SECRET-d41d8cd98f00b204e9800998ecf8427e"
	payload := json.RawMessage(`{"unexpectedField":"` + secret + `"}`)

	err := binance.AssertNoSecrets(payload, secret, "some-api-key")
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret,
		"the check that catches a leak must not become one")

	require.NoError(t, binance.AssertNoSecrets(json.RawMessage(`{"qty":"1"}`), secret, "some-api-key"))
}

func TestAssertNoSecretsIgnoresEmptyNeedles(t *testing.T) {
	// An unset credential env var must not make every payload look like a leak.
	require.NoError(t, binance.AssertNoSecrets(json.RawMessage(`{"a":"b"}`), "", ""))
}

func TestFixtureOutputIsIndentedForReview(t *testing.T) {
	got, err := binance.Fixture(json.RawMessage(`{"a":{"b":1}}`), "/api/v3/x")
	require.NoError(t, err)
	require.True(t, strings.Contains(string(got), "\n  "),
		"a fixture is read in review; it is written indented")
}
