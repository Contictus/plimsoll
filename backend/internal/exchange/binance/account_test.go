package binance_test

import (
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/stretchr/testify/require"
)

func spotAccountFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/fixtures/binance/spot_account.json")
	require.NoError(t, err)
	return raw
}

// The holding is free + locked.
//
// Comparing our fold against `free` alone would report every open order as a missing event:
// the balance sitting behind a resting limit order is held, not gone. It is the easiest
// available way to make reconciliation useless, and it fails in the direction that looks most
// like a real bug (F21).
func TestASpotHoldingIsFreePlusLocked(t *testing.T) {
	acct, err := binance.DecodeSpotAccount(spotAccountFixture(t))
	require.NoError(t, err)

	held := map[string]string{}
	for _, b := range acct.Balances {
		held[b.Asset] = b.Held().String()
	}

	require.Equal(t, "1.5", held["BTC"], "entirely free")
	require.Equal(t, "4.25", held["ETH"], "entirely locked behind an order, and still held")
	require.Equal(t, "1251", held["USDT"], "split across both")
}

// The venue's own instant, not ours (F21). It is what lets a snapshot be timestamped by the
// exchange, and the difference against our clock is the skew check from the coherence pass.
func TestTheSnapshotCarriesTheExchangesOwnInstant(t *testing.T) {
	acct, err := binance.DecodeSpotAccount(spotAccountFixture(t))
	require.NoError(t, err)

	require.Equal(t,
		time.UnixMilli(1789999200000).UTC(), acct.UpdateTime,
		"updateTime is milliseconds since the epoch, and it is the exchange's clock")
}

// Every number stays a string until it becomes a decimal. A balance decoded through float64
// comes back with different digits (L1), and the fixture's USDT value is chosen so that a
// float round trip would visibly lose a digit.
func TestNoBalanceIsParsedThroughAFloat(t *testing.T) {
	acct, err := binance.DecodeSpotAccount(spotAccountFixture(t))
	require.NoError(t, err)

	for _, b := range acct.Balances {
		if b.Asset == "USDT" {
			require.Equal(t, "1000.12345678", b.Free.String())
			require.Equal(t, "250.87654322", b.Locked.String())
			return
		}
	}
	t.Fatal("USDT missing from the fixture")
}

// A malformed number is an error, not a zero. A balance that silently becomes zero is the
// worst possible outcome here: it reports agreement we never measured.
func TestAMalformedBalanceIsRefusedRatherThanZeroed(t *testing.T) {
	_, err := binance.DecodeSpotAccount([]byte(
		`{"updateTime":1,"balances":[{"asset":"BTC","free":"not-a-number","locked":"0"}]}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "BTC", "the error must say which asset could not be read")
}
