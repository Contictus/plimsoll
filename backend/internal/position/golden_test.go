package position_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/position"
	"github.com/google/go-cmp/cmp"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

// The golden files are canonical events, not raw exchange payloads (D3): normalization is
// the exchange module's job in M2, and mixing the two would make a failure here ambiguous
// about which layer broke.
//
// Every number in them is a JSON string (L1), and each file carries the arithmetic worked
// out by hand so a reviewer can check the expected state without running anything. That is
// the point of a golden file -- if it only says what the code produced, it proves the code
// is deterministic and nothing about whether it is right.

type goldenFile struct {
	Name       string        `json:"name"`
	Why        string        `json:"why"`
	Arithmetic []string      `json:"arithmetic"`
	Events     []goldenEvent `json:"events"`
	Expected   goldenState   `json:"expected"`
}

type goldenEvent struct {
	VenueEventID  string `json:"venue_event_id"`
	VenueSequence int64  `json:"venue_sequence"`
	EventTime     string `json:"event_time"`
	EventType     string `json:"event_type"`
	Side          string `json:"side"`
	Quantity      string `json:"quantity"`
	Price         string `json:"price"`
	Fee           string `json:"fee"`
	FeeAsset      string `json:"fee_asset"`
}

type goldenState struct {
	Quantity      string      `json:"quantity"`
	AvgEntryPrice string      `json:"avg_entry_price"`
	RealizedPnL   string      `json:"realized_pnl"`
	Fees          []goldenFee `json:"fees"`
}

type goldenFee struct {
	Asset  string `json:"asset"`
	Amount string `json:"amount"`
}

// optional turns an absent JSON string into an absent decimal rather than into zero. A
// missing price and a price of zero are different claims, and the engine refuses the
// second one for a fill.
func optional(t *testing.T, s string) decimal.NullDecimal {
	t.Helper()
	if s == "" {
		return decimal.NullDecimal{}
	}
	d, err := decimal.NewFromString(s)
	require.NoError(t, err, "money must parse exactly: %q", s)
	return decimal.NewNullDecimal(d)
}

func (g goldenEvent) event(t *testing.T) ledger.Event {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, g.EventTime)
	require.NoError(t, err, "event_time %q", g.EventTime)

	return ledger.Event{
		VenueEventID:  g.VenueEventID,
		VenueSequence: g.VenueSequence,
		Source:        "golden",
		EventType:     ledger.EventType(g.EventType),
		Side:          ledger.Side(g.Side),
		Quantity:      optional(t, g.Quantity),
		Price:         optional(t, g.Price),
		Fee:           optional(t, g.Fee),
		FeeAsset:      g.FeeAsset,
		EventTime:     at,
	}
}

func observed(s position.State) goldenState {
	fees := make([]goldenFee, 0, len(s.Fees))
	for _, f := range s.Fees {
		fees = append(fees, goldenFee{Asset: f.Asset, Amount: f.Amount.String()})
	}
	return goldenState{
		Quantity:      s.Quantity.String(),
		AvgEntryPrice: s.AvgEntryPrice.String(),
		RealizedPnL:   s.RealizedPnL.String(),
		Fees:          fees,
	}
}

// TestGoldenFixturesReplay folds each file through Apply and compares the result with the
// state written into the file by hand. Comparison is on the decimal text, so 100 and
// 100.000 are a failure rather than a pass.
func TestGoldenFixturesReplay(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no golden fixtures found; the layout changed")

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)

			var golden goldenFile
			require.NoError(t, json.Unmarshal(raw, &golden))
			require.NotEmpty(t, golden.Arithmetic,
				"a golden file without its arithmetic proves determinism, not correctness")

			state := position.State{}
			for i, ge := range golden.Events {
				state, err = position.Apply(state, ge.event(t))
				require.NoError(t, err, "event %d (%s)", i, ge.VenueEventID)
			}

			require.Empty(t, cmp.Diff(golden.Expected, observed(state)),
				"%s: %s", golden.Name, golden.Why)
		})
	}
}
