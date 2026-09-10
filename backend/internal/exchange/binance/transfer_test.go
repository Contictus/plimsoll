package binance_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

const usdtAsset int64 = 88

// transfers returns the fixture's rows: [0] spot -> USD-M, [1] the way back, [2] not
// CONFIRMED, [3] a documented type outside the eight this milestone walks.
func transfers(t *testing.T) []json.RawMessage {
	t.Helper()
	var page struct {
		Total int               `json:"total"`
		Rows  []json.RawMessage `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(loadFixture(t, "universal_transfer.json", "payload"), &page))
	require.Len(t, page.Rows, 4)
	return page.Rows
}

func normalizeTransfer(t *testing.T, raw json.RawMessage) (ledger.Event, error) {
	t.Helper()
	tc := testContext()
	tc.Source = binance.SourceREST
	return binance.NormalizeTransfer(
		context.Background(), assetResolverFor("USDT", usdtAsset), tc, raw)
}

// One row is one event, and it carries both endpoints. The identity includes the type
// (F13): tranId is documented as an int64 per row and nothing on the page claims it is
// unique across directions, so a narrower id could merge two transfers into one.
func TestTransferMapsEveryField(t *testing.T) {
	raw := transfers(t)[0]

	event, err := normalizeTransfer(t, raw)
	require.NoError(t, err)

	require.Equal(t, ledger.TypeTransfer, event.EventType)
	require.Equal(t, "transfer:MAIN_UMFUTURE:11415955596", event.VenueEventID)
	require.Equal(t, "spot", event.TransferFrom)
	require.Equal(t, "usdm", event.TransferTo)
	require.NotNil(t, event.AssetID)
	require.Equal(t, usdtAsset, *event.AssetID)

	// Unsigned. The endpoints carry the direction, so a magnitude that was also a direction
	// would be two facts in one column, free to disagree with each other.
	require.True(t, event.Quantity.Decimal.Equal(decimal.RequireFromString("1")))

	// K2: the venue's own clock, in UTC. Everything downstream is calculated from it.
	require.Equal(t, time.UnixMilli(1544433328000).UTC(), event.EventTime.UTC())

	// A transfer is not a trade and not a fill.
	require.Nil(t, event.InstrumentID, "a transfer moves an asset, not a pair")
	require.False(t, event.Price.Valid, "a transfer has no price")
	require.Empty(t, event.Side)
	require.False(t, event.Fee.Valid, "no fee field is documented on this endpoint")

	require.Equal(t, []byte(raw), []byte(event.Raw))
}

// The direction is the whole of what distinguishes the two calls, so the reverse type must
// produce the reverse endpoints -- and a different identity, from the same tranId space.
func TestTheReverseTypeProducesTheReverseEndpoints(t *testing.T) {
	event, err := normalizeTransfer(t, transfers(t)[1])
	require.NoError(t, err)

	require.Equal(t, "usdm", event.TransferFrom)
	require.Equal(t, "spot", event.TransferTo)
	require.Equal(t, "transfer:UMFUTURE_MAIN:11366865406", event.VenueEventID)
}

// 32 types are documented and this milestone walks eight of them. A type outside that set
// must stop the normalizer rather than resolve to a plausible wallet: ISOLATEDMARGIN_MARGIN
// names an isolated-margin wallet this schema has no word for, and mapping it to `margin`
// would merge two wallets whose whole purpose is to be separate.
func TestATypeOutsideTheWalkedSetIsRefused(t *testing.T) {
	_, err := normalizeTransfer(t, transfers(t)[3])
	require.ErrorIs(t, err, binance.ErrUnknownTransferType)

	_, _, err = binance.WalletsOf("MAIN_INVENTED")
	require.ErrorIs(t, err, binance.ErrUnknownTransferType)
}

// F11: the status enum is not published. The page gives exactly one value, CONFIRMED, in
// its response example and enumerates nothing. So the normalizer whitelists rather than
// blacklists -- recording a transfer that has not happened misstates which wallet holds the
// money, which is precisely the question M5 asks.
func TestAStatusOtherThanConfirmedIsRefused(t *testing.T) {
	_, err := normalizeTransfer(t, transfers(t)[2])
	require.ErrorIs(t, err, binance.ErrTransferNotConfirmed)
}

// The eight directions this milestone walks, and their endpoints. Written as a table
// because the mapping IS the knowledge: an entry that is wrong here moves money between the
// wrong two wallets, and nothing downstream can tell.
func TestTheEightWalkedDirectionsMapToWallets(t *testing.T) {
	for _, tc := range []struct{ transferType, from, to string }{
		{"MAIN_UMFUTURE", "spot", "usdm"},
		{"UMFUTURE_MAIN", "usdm", "spot"},
		{"MAIN_CMFUTURE", "spot", "coinm"},
		{"CMFUTURE_MAIN", "coinm", "spot"},
		{"MAIN_MARGIN", "spot", "margin"},
		{"MARGIN_MAIN", "margin", "spot"},
		{"MAIN_FUNDING", "spot", "funding"},
		{"FUNDING_MAIN", "funding", "spot"},
	} {
		from, to, err := binance.WalletsOf(tc.transferType)
		require.NoError(t, err, tc.transferType)
		require.Equal(t, tc.from, from, tc.transferType)
		require.Equal(t, tc.to, to, tc.transferType)
	}
}

// A row that cannot be identified, timed, or measured is refused rather than stored with a
// hole in it. Each of these would fold into a wrong number quietly: an id of zero collides
// with the next such row, a missing timestamp lands the event at the Unix epoch and reorders
// the whole ledger (L7), and a non-positive amount is a direction trying to travel in the
// column the endpoints already own.
func TestAnUnusableTransferRowIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, row string }{
		{"no transaction id", `{"asset":"USDT","amount":"1","type":"MAIN_UMFUTURE","status":"CONFIRMED","tranId":0,"timestamp":1544433328000}`},
		{"no asset", `{"asset":"","amount":"1","type":"MAIN_UMFUTURE","status":"CONFIRMED","tranId":1,"timestamp":1544433328000}`},
		{"no timestamp", `{"asset":"USDT","amount":"1","type":"MAIN_UMFUTURE","status":"CONFIRMED","tranId":1,"timestamp":0}`},
		{"zero amount", `{"asset":"USDT","amount":"0","type":"MAIN_UMFUTURE","status":"CONFIRMED","tranId":1,"timestamp":1544433328000}`},
		{"negative amount", `{"asset":"USDT","amount":"-1","type":"MAIN_UMFUTURE","status":"CONFIRMED","tranId":1,"timestamp":1544433328000}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeTransfer(t, json.RawMessage(tc.row))
			require.Error(t, err, "a transfer with %s was normalized", tc.name)
		})
	}
}

// The fixture is documented rather than recorded, and it must stay that way in one specific
// respect: a real transfer row carries no address and no txId, so there is nothing here that
// could identify a wallet even if the fixture were later replaced by a recording (L13).
func TestTheTransferFixtureCarriesNothingIdentifying(t *testing.T) {
	for _, raw := range transfers(t) {
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &fields))
		for _, forbidden := range []string{"address", "addressTag", "txId", "email", "uid"} {
			require.NotContains(t, fields, forbidden)
		}
	}
}

// The backfill will walk one query per direction (F10), and it reads the directions from
// here rather than keeping its own list. A second list is a second place for the walk and
// the normalizer to disagree about which transfers exist -- and the symptom of that
// disagreement is transfers that are never fetched, which look exactly like transfers that
// never happened.
func TestTheWalkedTypesAreTheTypesTheNormalizerKnows(t *testing.T) {
	types := binance.WalkedTransferTypes()
	require.Len(t, types, 8)
	require.Equal(t, "CMFUTURE_MAIN", types[0], "the list must be sorted, so a walk is deterministic")

	for _, transferType := range types {
		from, to, err := binance.WalletsOf(transferType)
		require.NoError(t, err, transferType)
		require.NotEqual(t, from, to, "%s runs between one wallet and itself", transferType)
	}
}
