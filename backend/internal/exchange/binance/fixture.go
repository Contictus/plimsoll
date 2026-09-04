package binance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RedactedPlaceholder replaces every identifying value. A visible marker rather than an
// empty string, so a reader of the fixture can tell "removed on purpose" from "the
// exchange sent nothing".
const RedactedPlaceholder = "REDACTED"

// FixtureSourceRecorded is the _source value for a payload captured from a real account.
// The taxonomy lives in testdata/fixtures/binance/README.md.
const FixtureSourceRecorded = "recorded"

// redactedFields are the response fields that identify a person or a wallet rather than a
// trade. Matched case-insensitively, because a field spelled Address must not survive on a
// technicality.
//
// Trade, order and transfer ids are deliberately absent: they are event identity (L5), a
// fixture without them cannot exercise deduplication, and they identify a transaction
// rather than the account that made it.
var redactedFields = map[string]bool{
	"address":      true,
	"addresstag":   true,
	"txid":         true,
	"txkey":        true,
	"info":         true,
	"email":        true,
	"uid":          true,
	"accountalias": true,
	"apikey":       true,
	"listenkey":    true,
}

// Redact returns raw with every identifying field's value replaced, at any depth, in both
// objects and arrays. Numbers are carried through as their original text (json.Number), so
// a quantity is never routed through float64 and rewritten -- a fixture is a money fixture,
// and every later test is checked against these digits (L1).
//
// It is applied before the bytes reach disk, never afterwards: "record now, redact before
// committing" is how a real address reaches git history, where deleting it does not remove
// it.
func Redact(raw json.RawMessage) (json.RawMessage, error) {
	decoded, err := decodePreservingNumbers(raw)
	if err != nil {
		return nil, err
	}
	return marshalIndented(redactValue(decoded))
}

// Fixture redacts raw and annotates it with where it came from. An object carries the
// annotation inline, the way every existing fixture does; an array has nowhere to put a
// key, so it is wrapped under "payload" rather than being written with no provenance at
// all.
func Fixture(raw json.RawMessage, endpoint string) ([]byte, error) {
	decoded, err := decodePreservingNumbers(raw)
	if err != nil {
		return nil, err
	}
	redacted := redactValue(decoded)

	meta := map[string]any{
		"_source":      FixtureSourceRecorded,
		"_endpoint":    endpoint,
		"_recorded_at": time.Now().UTC().Format(time.RFC3339),
		"_why":         "recorded from a real read-only key by plimsollctl record",
		"_redaction":   "identifying fields replaced in the write path; amounts untouched",
	}

	if object, ok := redacted.(map[string]any); ok {
		for key, value := range meta {
			object[key] = value
		}
		return marshalIndentedBytes(object)
	}
	meta["payload"] = redacted
	return marshalIndentedBytes(meta)
}

// AssertNoSecrets is the last gate before bytes reach disk. Redaction works from a list of
// field names, and a list is a thing Binance can outgrow; this check does not care what a
// field is called, only that the credential is not inside the payload.
//
// The error names neither the secret nor where it was found (L13): a check that reports a
// leak by quoting it is a leak.
func AssertNoSecrets(payload []byte, secrets ...string) error {
	for _, secret := range secrets {
		if secret == "" {
			// An unset credential must not make every payload look like a leak.
			continue
		}
		if bytes.Contains(payload, []byte(secret)) {
			return errors.New("binance: refusing to write a fixture containing the credential")
		}
	}
	return nil
}

// decodePreservingNumbers decodes into any, keeping every number as its original text.
// encoding/json otherwise turns them into float64, which loses digits on a NUMERIC(38,18)
// quantity and on trade ids past 2^53 (L1).
func decodePreservingNumbers(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("binance: response is not json, refusing to write it: %w", err)
	}
	return decoded, nil
}

// redactValue walks the decoded payload. Values are replaced rather than keys deleted:
// dropping the key would change the shape, and a parser tested against the changed shape
// is not tested against Binance's.
func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if redactedFields[strings.ToLower(key)] {
				out[key] = RedactedPlaceholder
				continue
			}
			out[key] = redactValue(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = redactValue(child)
		}
		return out
	default:
		return value
	}
}

func marshalIndented(value any) (json.RawMessage, error) {
	out, err := marshalIndentedBytes(value)
	return json.RawMessage(out), err
}

// marshalIndentedBytes writes the fixture the way it will be read: indented, in review, by
// a human. HTML escaping is off because a redacted payload has no HTML in it and the
// escapes would only make the diff harder to read.
func marshalIndentedBytes(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("binance: encode fixture: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
