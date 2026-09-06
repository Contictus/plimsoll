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

const bnbAsset int64 = 77

// deposits returns the fixture's rows: [0] settled, [1] pending, [2] credited but locked.
func deposits(t *testing.T) []json.RawMessage {
	t.Helper()
	var rows []json.RawMessage
	require.NoError(t, json.Unmarshal(loadFixture(t, "deposits.json", "payload"), &rows))
	require.Len(t, rows, 3)
	return rows
}

func TestDepositMapsEveryField(t *testing.T) {
	tc := testContext()
	tc.Source = binance.SourceREST
	raw := deposits(t)[0]

	event, err := binance.NormalizeDeposit(
		context.Background(), assetResolverFor("BNB", bnbAsset), tc, raw)
	require.NoError(t, err)

	require.Equal(t, ledger.TypeDeposit, event.EventType)
	require.Equal(t, "spot:deposit:769800519366885376", event.VenueEventID)
	require.NotNil(t, event.AssetID)
	require.Equal(t, bnbAsset, *event.AssetID)
	require.True(t, event.Quantity.Decimal.Equal(decimal.RequireFromString("0.001")))
	require.Equal(t, time.UnixMilli(1661493146000).UTC(), event.EventTime.UTC())

	// A deposit is not a trade. It has no counterparty price, no side, and no instrument:
	// giving it a price of zero would hand it a cost basis it never had (K18).
	require.False(t, event.Price.Valid, "a deposit has no price")
	require.Nil(t, event.InstrumentID, "a deposit moves an asset, not a pair")
	require.Empty(t, event.Side)
	require.False(t, event.Fee.Valid, "the deposit endpoint reports no fee")

	require.Equal(t, []byte(raw), []byte(event.Raw))
}

// The identity is the venue's own record id, never the chain hash. txId links the account
// to a wallet -- it is redacted out of fixtures -- and an internal transfer has no txId at
// all, so keying on it would give some deposits no identity and others a shared one.
func TestDepositIdentityUsesTheRecordIDNotTheChainHash(t *testing.T) {
	tc := testContext()
	raw := deposits(t)[0]

	event, err := binance.NormalizeDeposit(
		context.Background(), assetResolverFor("BNB", bnbAsset), tc, raw)
	require.NoError(t, err)

	require.Equal(t, "spot:deposit:769800519366885376", event.VenueEventID)
	require.NotContains(t, event.VenueEventID, "98A3EA560C6B3336",
		"the chain hash must not be part of the identity")
}

// A pending deposit is not money in the account. Recording one would credit a balance that
// may never arrive, and the ledger is append-only -- the correction would have to be a
// second event reversing the first (L2).
func TestPendingDepositIsNotAnEvent(t *testing.T) {
	tc := testContext()
	_, err := binance.NormalizeDeposit(
		context.Background(), assetResolverFor("USDT", 78), tc, deposits(t)[1])
	require.ErrorIs(t, err, binance.ErrNotSettled)
}

// "Credited but cannot withdraw" is credited. The coins are in the account and count
// toward the portfolio; only moving them out is blocked, which is not the ledger's concern.
func TestCreditedButLockedDepositIsAnEvent(t *testing.T) {
	tc := testContext()
	event, err := binance.NormalizeDeposit(
		context.Background(), assetResolverFor("USDT", 78), tc, deposits(t)[2])
	require.NoError(t, err)
	require.True(t, event.Quantity.Decimal.Equal(decimal.RequireFromString("5")))
}

// Every status the documentation lists, and only those. Rejected, wrong and
// waiting-for-confirmation all mean the money is not in the account.
func TestOnlyDocumentedStatusesAreAccepted(t *testing.T) {
	tc := testContext()
	settled := deposits(t)[0]

	for _, tc2 := range []struct {
		status int
		settle bool
	}{
		{0, false}, // pending
		{1, true},  // success
		{2, false}, // rejected
		{6, true},  // credited but cannot withdraw
		{7, false}, // wrong deposit
		{8, false}, // waiting user confirm
	} {
		raw := mutateJSON(t, settled, map[string]any{"status": tc2.status})
		_, err := binance.NormalizeDeposit(
			context.Background(), assetResolverFor("BNB", bnbAsset), tc, raw)
		if tc2.settle {
			require.NoError(t, err, "status %d", tc2.status)
			continue
		}
		require.ErrorIs(t, err, binance.ErrNotSettled, "status %d", tc2.status)
	}
}

// A status code we have never seen must stop the ingest, not be filed under "not settled".
// Guessing wrong in the settled direction credits money that was never received; guessing
// wrong in the other direction loses a deposit and puts a hole in the cost basis (K26).
func TestUnknownDepositStatusIsRefusedRatherThanSkipped(t *testing.T) {
	tc := testContext()
	raw := mutateJSON(t, deposits(t)[0], map[string]any{"status": 99})

	_, err := binance.NormalizeDeposit(
		context.Background(), assetResolverFor("BNB", bnbAsset), tc, raw)
	require.Error(t, err)
	require.NotErrorIs(t, err, binance.ErrNotSettled,
		"an undocumented status is not a known non-settlement")
	require.Contains(t, err.Error(), "99")
}

// L8 applies to assets exactly as it does to instruments: an exchange ticker is resolved as
// of the event's own time, because exchanges recycle tickers after a delisting.
func TestDepositAssetIsResolvedAtTheEventsOwnTime(t *testing.T) {
	depositTime := time.UnixMilli(1661493146000).UTC()
	resolver := &fakeResolver{assetWindows: []aliasWindow{
		{"BNB", time.Unix(0, 0), depositTime.Add(time.Hour), 101},
		{"BNB", depositTime.Add(time.Hour), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), 202},
	}}

	tc := testContext()
	event, err := binance.NormalizeDeposit(context.Background(), resolver, tc, deposits(t)[0])
	require.NoError(t, err)

	require.Equal(t, int64(101), *event.AssetID,
		"resolved with the wrong window: a 2022 deposit must not get today's mapping")
	require.Len(t, resolver.assetCalls, 1)
	require.Equal(t, depositTime, resolver.assetCalls[0].at.UTC())
}

func TestUnresolvableCoinIsAnErrorNamingTheCoin(t *testing.T) {
	tc := testContext()
	_, err := binance.NormalizeDeposit(
		context.Background(), assetResolverFor("ETH", 5), tc, deposits(t)[0])
	require.Error(t, err)
	require.Contains(t, err.Error(), "BNB")
}

// A deposit with no id has no identity, and one with no amount is not a deposit. Both are
// refused rather than stored with a placeholder.
func TestMalformedDepositIsRefused(t *testing.T) {
	tc := testContext()
	for name, patch := range map[string]map[string]any{
		"no record id":  {"id": ""},
		"no amount":     {"amount": ""},
		"zero amount":   {"amount": "0"},
		"no coin":       {"coin": ""},
		"no insertTime": {"insertTime": 0},
	} {
		t.Run(name, func(t *testing.T) {
			raw := mutateJSON(t, deposits(t)[0], patch)
			_, err := binance.NormalizeDeposit(
				context.Background(), assetResolverFor("BNB", bnbAsset), tc, raw)
			require.Error(t, err)
			require.NotErrorIs(t, err, binance.ErrNotSettled)
		})
	}
}

// L1: a deposit amount is money. Eighteen decimal places must survive it.
func TestDepositAmountSurvivesEighteenDecimalPlaces(t *testing.T) {
	const amount = "0.123456789012345678"
	tc := testContext()
	raw := mutateJSON(t, deposits(t)[0], map[string]any{"amount": amount})

	event, err := binance.NormalizeDeposit(
		context.Background(), assetResolverFor("BNB", bnbAsset), tc, raw)
	require.NoError(t, err)
	require.Equal(t, amount, event.Quantity.Decimal.String())
}
