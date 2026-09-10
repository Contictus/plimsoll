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

// transfer is one asset moving between two named wallets. Quantity is unsigned: the two
// endpoints carry the direction (F10).
func transfer(from, to, quantity string) ledger.Event {
	asset := usdt
	return ledger.Event{
		VenueEventID: "transfer:MAIN_UMFUTURE:1",
		EventType:    ledger.TypeTransfer,
		AssetID:      &asset,
		TransferFrom: from,
		TransferTo:   to,
		Quantity:     dec(quantity),
	}
}

// fold accumulates the deltas of several events the way the projector does, so a test can
// state what an account holds after a sequence rather than after one event.
func fold(t *testing.T, events []ledger.Event, resolved []balance.Resolved) map[int64]string {
	t.Helper()
	held := map[int64]decimal.Decimal{}
	for i, e := range events {
		deltas, err := balance.Deltas(e, resolved[i])
		require.NoError(t, err)
		for _, d := range deltas {
			held[d.AssetID] = held[d.AssetID].Add(d.Amount)
		}
	}
	out := map[int64]string{}
	for id, amount := range held {
		out[id] = amount.String()
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

// THE TEST THIS MILESTONE EXISTS FOR.
//
// asset_balances is keyed per integration, not per wallet. Moving USDT from the spot wallet
// to the futures wallet of one account does not change what that account holds, so the fold
// produces nothing at all. The event stays in the ledger because it is history and lineage
// -- it is simply not arithmetic.
//
// The failure this prevents is the one the milestone is named after: a transfer out read as
// a disposal, which invents a realized loss and then a phantom re-purchase when the money
// comes back.
func TestATransferBetweenTwoWalletsOfOneIntegrationMovesNothing(t *testing.T) {
	for _, tc := range [][2]string{
		{"spot", "usdm"},
		{"usdm", "spot"},
		{"spot", "funding"},
		{"margin", "coinm"},
	} {
		got, err := balance.Deltas(transfer(tc[0], tc[1], "500"), balance.Resolved{})
		require.NoError(t, err)
		require.Empty(t, moved(t, got), "%s -> %s moved something", tc[0], tc[1])
	}
}

// The branch M8 turns on. One side outside this integration is a real movement, and which
// side it is supplies the direction -- the quantity itself is unsigned, because with both
// endpoints named a sign would be a second statement of the same fact.
//
// Written now rather than with M8, because a rule that only ever ran on the internal case
// would have quietly become "a transfer moves nothing", and the first cross-venue withdrawal
// would have vanished.
func TestATransferWithAnExternalSideMovesTheBalance(t *testing.T) {
	out, err := balance.Deltas(transfer("spot", "external", "500"), balance.Resolved{})
	require.NoError(t, err)
	require.Equal(t, [][2]string{{"2", "-500"}}, moved(t, out), "money that left is still held")

	in, err := balance.Deltas(transfer("external", "spot", "500"), balance.Resolved{})
	require.NoError(t, err)
	require.Equal(t, [][2]string{{"2", "500"}}, moved(t, in), "money that arrived was not credited")
}

// THE SELLER'S FALLACY, stated as arithmetic.
//
// Move 500 USDT to the futures wallet, then buy with what is left. The balance must be
// exactly what the fills alone would leave: the transfer is not a sale, so it neither spends
// the asset nor makes the later fills unaffordable.
func TestATransferOutFollowedByFillsLeavesWhatTheFillsAloneWouldLeave(t *testing.T) {
	buy := fill(ledger.SideBuy, "0.01", "60000")

	withTransfer := fold(t,
		[]ledger.Event{transfer("spot", "usdm", "500"), buy},
		[]balance.Resolved{{}, legs()})
	fillsAlone := fold(t,
		[]ledger.Event{buy},
		[]balance.Resolved{legs()})

	require.Equal(t, fillsAlone, withTransfer,
		"the transfer changed the balance the fills produced")
	require.Equal(t, map[int64]string{btc: "0.01", usdt: "-600"}, withTransfer)
}

// A transfer naming one endpoint cannot be folded: whether the money moved is exactly the
// comparison between the two, and with one of them missing there is nothing to compare.
// The schema refuses this shape too (00020); the engine refuses it because a pure function
// that trusts its caller to have a constraint is not pure, it is lucky.
func TestATransferMissingAnEndpointIsRefused(t *testing.T) {
	for _, tc := range [][2]string{{"", ""}, {"spot", ""}, {"", "usdm"}} {
		_, err := balance.Deltas(transfer(tc[0], tc[1], "500"), balance.Resolved{})
		require.ErrorIs(t, err, balance.ErrMalformedEvent, "%q -> %q was folded", tc[0], tc[1])
	}
}

// Both sides outside this integration is not a movement of this account's money, and it is
// not an internal transfer either -- it is a normalizer that lost track of which side of the
// wire it was on. Folding it to nothing would hide that.
func TestATransferBetweenTwoOutsidesIsRefused(t *testing.T) {
	_, err := balance.Deltas(transfer("external", "external", "500"), balance.Resolved{})
	require.ErrorIs(t, err, balance.ErrMalformedEvent)
}

// A negative quantity is the retired convention showing up again. With the direction on the
// endpoints, a sign can only disagree with them, and the disagreement would be silent: a
// withdrawal of -500 would read as money arriving.
func TestATransferWithASignedQuantityIsRefused(t *testing.T) {
	_, err := balance.Deltas(transfer("spot", "external", "-500"), balance.Resolved{})
	require.ErrorIs(t, err, balance.ErrMalformedEvent)
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

// Funding is a cash flow in the settle asset, and V1 is USD-M only, so the settle asset is
// the quote asset and this is exact rather than an approximation.
//
// Signed both ways, because it goes both ways: a short position in a positive-funding market
// is PAID. A fold that took the absolute value would turn every funding receipt into a cost
// and quietly understate the account by twice the funding it earned.
func TestFundingMovesTheSettleAssetInWhicheverDirectionItWent(t *testing.T) {
	for _, tc := range []struct{ income, want string }{
		{"-0.375", "-0.375"},
		{"1.25", "1.25"},
		{"0", "0"},
	} {
		e := ledger.Event{
			VenueEventID: "usdm:income:FUNDING_FEE:1",
			EventType:    ledger.TypeFundingPayment,
			Quantity:     dec(tc.income),
		}
		got, err := balance.Deltas(e, legs())
		require.NoError(t, err)
		require.Equal(t, [][2]string{{"2", tc.want}}, moved(t, got),
			"funding of %s moved the wrong asset or the wrong way", tc.income)
	}
}

// Funding without an instrument cannot be folded: the settle asset is a property of the
// contract, and a payment with no contract has no asset to move. Guessing one is how a
// funding cost lands on a coin the account never held.
func TestFundingWithoutLegsIsRefused(t *testing.T) {
	_, err := balance.Deltas(ledger.Event{
		VenueEventID: "usdm:income:FUNDING_FEE:1",
		EventType:    ledger.TypeFundingPayment,
		Quantity:     dec("-1"),
	}, balance.Resolved{})
	require.ErrorIs(t, err, balance.ErrUnresolved)
}

// Funding never touches the average entry price -- that is internal/position's rule (K18)
// and it is tested there. What this engine must get right is the other half: the money did
// leave the account, so the balance moves even though the cost basis does not.
func TestFundingChangesTheBalanceWhileTheEntryPriceIsSomeoneElsesProblem(t *testing.T) {
	buy := fill(ledger.SideBuy, "1", "100")
	funding := ledger.Event{
		VenueEventID: "usdm:income:FUNDING_FEE:2",
		EventType:    ledger.TypeFundingPayment,
		Quantity:     dec("-3"),
	}

	held := fold(t, []ledger.Event{buy, funding}, []balance.Resolved{legs(), legs()})
	require.Equal(t, map[int64]string{btc: "1", usdt: "-103"}, held,
		"the 100 the fill spent plus the 3 the funding cost")
}
