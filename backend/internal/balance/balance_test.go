package balance_test

import (
	"fmt"
	"testing"

	"github.com/Contictus/plimsoll/backend/internal/balance"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

const (
	btc  int64 = 1
	usdt int64 = 2
	bnb  int64 = 3
)

func dec(s string) decimal.NullDecimal {
	return decimal.NewNullDecimal(decimal.RequireFromString(s))
}

func legs() balance.Resolved {
	return balance.Resolved{Legs: &balance.Legs{BaseAssetID: btc, QuoteAssetID: usdt}}
}

func withFee(r balance.Resolved, id int64) balance.Resolved {
	r.FeeAssetID = &id
	return r
}

// moved is the deltas as (asset, amount) pairs of strings. Compared this way rather than as
// decimal values, because a rounded decimal and a literal one are the same number and
// different structs -- and it is the number this engine is responsible for.
func moved(t *testing.T, deltas []balance.Delta) [][2]string {
	t.Helper()
	out := make([][2]string, 0, len(deltas))
	for _, d := range deltas {
		out = append(out, [2]string{fmt.Sprint(d.AssetID), d.Amount.String()})
	}
	return out
}

func fill(side ledger.Side, quantity, price string) ledger.Event {
	return ledger.Event{
		VenueEventID: "spot:trade:BTCUSDT:1",
		EventType:    ledger.TypeTrade,
		Side:         side,
		Quantity:     dec(quantity),
		Price:        dec(price),
	}
}

// A buy acquires the base and spends the quote. Both legs move, which is the whole reason
// this engine exists next to internal/position: a position says the exposure cost 60000, a
// balance says the 60000 is gone.
func TestABuyAcquiresTheBaseAndSpendsTheQuote(t *testing.T) {
	got, err := balance.Deltas(fill(ledger.SideBuy, "0.5", "60000"), legs())
	require.NoError(t, err)

	require.Equal(t, [][2]string{{"1", "0.5"}, {"2", "-30000"}}, moved(t, got))
}

func TestASellSpendsTheBaseAndAcquiresTheQuote(t *testing.T) {
	got, err := balance.Deltas(fill(ledger.SideSell, "0.5", "60000"), legs())
	require.NoError(t, err)

	require.Equal(t, [][2]string{{"1", "-0.5"}, {"2", "30000"}}, moved(t, got))
}

// The fee is a movement here, unlike in internal/position where it is deliberately kept out
// of the entry price (K18, L9). Both are right: a fee never changes what a position cost,
// and it certainly changes what the account holds.
func TestTheFeeIsSpentFromItsOwnAsset(t *testing.T) {
	e := fill(ledger.SideBuy, "0.5", "60000")
	e.Fee, e.FeeAsset = dec("0.01"), "BNB"

	got, err := balance.Deltas(e, withFee(legs(), bnb))
	require.NoError(t, err)

	require.Len(t, got, 3)
	require.Equal(t, [2]string{"3", "-0.01"}, moved(t, got)[2])
}

// A rebate arrives as a negative fee, so one subtraction handles both and they never have
// to be added together later.
func TestARebateArrivesAsANegativeFeeAndAddsToTheBalance(t *testing.T) {
	e := ledger.Event{
		VenueEventID: "spot:rebate:1",
		EventType:    ledger.TypeCommissionRebate,
		Fee:          dec("-0.02"),
		FeeAsset:     "BNB",
	}

	got, err := balance.Deltas(e, withFee(balance.Resolved{}, bnb))
	require.NoError(t, err)
	require.Equal(t, [][2]string{{"3", "0.02"}}, moved(t, got))
}

// A fee whose asset did not resolve is refused rather than dropped. Dropping it would leave
// a balance short by exactly one fee with nothing to say so, which is the quiet shortfall
// L11 exists to prevent -- the caller has to decide what to do about it.
func TestAFeeWhoseAssetDidNotResolveIsRefused(t *testing.T) {
	e := fill(ledger.SideBuy, "0.5", "60000")
	e.Fee, e.FeeAsset = dec("0.01"), "NEWCOIN"

	_, err := balance.Deltas(e, legs())
	require.ErrorIs(t, err, balance.ErrUnresolved)
	require.Contains(t, err.Error(), "NEWCOIN")
}

// A withdrawal states what left, positively. The event type carries the direction; the
// number does not, and reading the sign off the number instead would make a deposit and a
// withdrawal indistinguishable.
func TestADepositAddsAndAWithdrawalSubtractsTheSameNumber(t *testing.T) {
	for _, tc := range []struct {
		eventType ledger.EventType
		want      string
	}{
		{ledger.TypeDeposit, "3"},
		{ledger.TypeWithdrawal, "-3"},
	} {
		asset := btc
		e := ledger.Event{
			VenueEventID: "spot:" + string(tc.eventType) + ":1",
			EventType:    tc.eventType,
			AssetID:      &asset,
			Quantity:     dec("3"),
		}
		got, err := balance.Deltas(e, balance.Resolved{})
		require.NoError(t, err)
		require.Equal(t, tc.want, got[0].Amount.String(), "%s", tc.eventType)
	}
}

// A transfer's quantity is signed, because one transfer is one movement seen from two
// sides. Nothing produces these yet; this is the convention the K12 normalizer must honour,
// written where the fold can enforce it rather than left to be rediscovered.
func TestATransferReadsItsDirectionFromTheSign(t *testing.T) {
	asset := usdt
	out := ledger.Event{
		VenueEventID: "spot:transfer:1", EventType: ledger.TypeTransfer,
		AssetID: &asset, Quantity: dec("-500"),
	}
	got, err := balance.Deltas(out, balance.Resolved{})
	require.NoError(t, err)
	require.Equal(t, "-500", got[0].Amount.String())
}

// Better a halted fold than a quietly incomplete one -- the same rule internal/position
// follows, for the same reason.
func TestAnEventTypeWithNoRuleIsRefused(t *testing.T) {
	_, err := balance.Deltas(ledger.Event{
		VenueEventID: "x", EventType: ledger.TypePositionAdjustment,
	}, balance.Resolved{})
	require.ErrorIs(t, err, balance.ErrUnsupportedEventType)
}

// A fill with no legs cannot be folded: guessing which assets moved is the identity error
// K10 exists to prevent, in its most direct form.
func TestAFillWithoutLegsIsRefused(t *testing.T) {
	_, err := balance.Deltas(fill(ledger.SideBuy, "1", "100"), balance.Resolved{})
	require.ErrorIs(t, err, balance.ErrUnresolved)
}
