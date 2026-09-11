package bybit_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/exchange/bybit"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// resolver maps a coin to an asset id and records what it was asked, so a test can assert
// that resolution happened at the EVENT's time rather than at the wall clock (L8).
type resolver struct {
	askedAt map[string]time.Time
	fail    bool
}

func (r *resolver) Asset(_ context.Context, symbol string, at time.Time) (int64, error) {
	if r.askedAt == nil {
		r.askedAt = map[string]time.Time{}
	}
	r.askedAt[symbol] = at
	if r.fail {
		return 0, errors.New("no alias covers this coin at this time")
	}
	return int64(len(symbol)), nil
}

func ingestContext() bybit.IngestContext {
	return bybit.IngestContext{
		AccountID: uuid.New(), IntegrationID: uuid.New(), Source: "rest",
	}
}

func rowsOf(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	page, err := bybit.DecodePage(fixture(t, name))
	require.NoError(t, err)
	return page.Rows
}

// A settled deposit becomes a DEPOSIT event with a positive quantity and no price.
func TestASettledDepositBecomesALedgerEvent(t *testing.T) {
	rows := rowsOf(t, "deposit_records.json")
	r := &resolver{}

	got, err := bybit.NormalizeDeposit(context.Background(), r, ingestContext(), rows[0])
	require.NoError(t, err)

	require.Equal(t, ledger.TypeDeposit, got.EventType)
	require.Equal(t, "bybit:deposit:dep-1", got.VenueEventID)
	require.Equal(t, "500", got.Quantity.Decimal.String())
	require.True(t, got.Quantity.Decimal.IsPositive(),
		"direction lives on the event type, never on the sign (K49)")
	require.Nil(t, got.InstrumentID, "a deposit moves one asset; it has no pair and no price")
	require.Equal(t, time.UnixMilli(1789999200000).UTC(), got.EventTime)
	require.JSONEq(t, string(rows[0]), string(got.Raw), "the venue's bytes are kept verbatim (L15)")
}

// 70012 -- "remains successful after rollback review" -- IS settled, and 70011 -- "rolled
// back" -- is not. They differ by one digit and by the whole answer (B2).
func TestTheTwoRollbackStatusesAreNotTheSame(t *testing.T) {
	rows := rowsOf(t, "deposit_records.json")
	r := &resolver{}

	reviewed, err := bybit.NormalizeDeposit(context.Background(), r, ingestContext(), rows[1])
	require.NoError(t, err, "70012 is money that stayed")
	require.Equal(t, "125.5", reviewed.Quantity.Decimal.String())

	_, err = bybit.NormalizeDeposit(context.Background(), r, ingestContext(), rows[2])
	require.ErrorIs(t, err, bybit.ErrNotSettled, "70011 is money that went back")
}

// A deposit still processing is not an error in the record -- it is an answer about a movement
// that has not happened yet, and folding it would credit money that has not arrived.
func TestAProcessingDepositIsNotSettled(t *testing.T) {
	rows := rowsOf(t, "deposit_records.json")
	_, err := bybit.NormalizeDeposit(context.Background(), &resolver{}, ingestContext(), rows[3])
	require.ErrorIs(t, err, bybit.ErrNotSettled)
}

// A settled withdrawal becomes a WITHDRAWAL event. This normalizer exists and its Binance
// counterpart does not, because Bybit publishes the enum that decides it and Binance does not
// (B2, F5).
func TestASettledWithdrawalBecomesALedgerEvent(t *testing.T) {
	rows := rowsOf(t, "withdraw_records.json")

	got, err := bybit.NormalizeWithdrawal(context.Background(), &resolver{}, ingestContext(), rows[0])
	require.NoError(t, err)

	require.Equal(t, ledger.TypeWithdrawal, got.EventType)
	require.Equal(t, "bybit:withdrawal:wd-1", got.VenueEventID)
	require.Equal(t, "500.5", got.Quantity.Decimal.String())
	require.True(t, got.Quantity.Decimal.IsPositive())
	require.Equal(t, time.UnixMilli(1789999200000).UTC(), got.EventTime)
}

// `BlockchainConfirmed` is NOT `success`.
//
// The chain having confirmed the transaction is not the same claim as the venue having
// finished the withdrawal, and the enum lists both -- so treating them as one reads a
// distinction the venue drew and decides it did not mean it.
func TestBlockchainConfirmedIsNotSuccess(t *testing.T) {
	rows := rowsOf(t, "withdraw_records.json")
	_, err := bybit.NormalizeWithdrawal(context.Background(), &resolver{}, ingestContext(), rows[1])
	require.ErrorIs(t, err, bybit.ErrNotSettled)
}

// A rejected withdrawal never left.
func TestARejectedWithdrawalIsNotSettled(t *testing.T) {
	rows := rowsOf(t, "withdraw_records.json")
	_, err := bybit.NormalizeWithdrawal(context.Background(), &resolver{}, ingestContext(), rows[2])
	require.ErrorIs(t, err, bybit.ErrNotSettled)
}

// A status outside the published enum is REFUSED, in both directions.
//
// Which status means "the coins moved" decides whether a balance is right. A new code mapped
// to the nearest plausible meaning is a wrong balance that looks exactly like a right one, and
// it would be written into rows that are never updated (L2).
func TestAStatusOutsideTheEnumIsRefused(t *testing.T) {
	_, err := bybit.NormalizeDeposit(context.Background(), &resolver{}, ingestContext(),
		json.RawMessage(`{"id":"x","coin":"USDT","amount":"1","status":424242,"successAt":"1789999200000"}`))
	require.ErrorIs(t, err, bybit.ErrUnknownStatus)

	_, err = bybit.NormalizeWithdrawal(context.Background(), &resolver{}, ingestContext(),
		json.RawMessage(`{"withdrawId":"x","coin":"USDT","amount":"1","status":"SomethingNew","updateTime":"1789999200000"}`))
	require.ErrorIs(t, err, bybit.ErrUnknownStatus)
}

// A missing status is not status zero. Defaulting it would decide the one question the record
// exists to answer, by accident.
func TestAMissingDepositStatusIsRefused(t *testing.T) {
	_, err := bybit.NormalizeDeposit(context.Background(), &resolver{}, ingestContext(),
		json.RawMessage(`{"id":"x","coin":"USDT","amount":"1","successAt":"1789999200000"}`))
	require.ErrorIs(t, err, bybit.ErrMalformed)
}

// The coin is resolved as of the EVENT's own time, never now. A symbol recycled after a
// delisting attaches a correct quantity to the wrong asset otherwise (L8, K22).
func TestTheCoinIsResolvedAtTheEventsOwnTime(t *testing.T) {
	rows := rowsOf(t, "deposit_records.json")
	r := &resolver{}

	_, err := bybit.NormalizeDeposit(context.Background(), r, ingestContext(), rows[0])
	require.NoError(t, err)
	require.Equal(t, time.UnixMilli(1789999200000).UTC(), r.askedAt["USDT"])
}

// An unresolvable coin fails the normalization rather than producing an event with no asset.
func TestAnUnresolvableCoinFailsRatherThanLosingTheAsset(t *testing.T) {
	rows := rowsOf(t, "deposit_records.json")
	_, err := bybit.NormalizeDeposit(
		context.Background(), &resolver{fail: true}, ingestContext(), rows[0])
	require.Error(t, err)
}

// A record with no id has no identity, and identity is the whole of L5's dedup key.
func TestARecordWithNoIdIsRefused(t *testing.T) {
	_, err := bybit.NormalizeDeposit(context.Background(), &resolver{}, ingestContext(),
		json.RawMessage(`{"coin":"USDT","amount":"1","status":3,"successAt":"1789999200000"}`))
	require.ErrorIs(t, err, bybit.ErrMalformed)
}

// A negative amount is refused: with the direction on the event type, a sign is a second
// statement of the same fact and free to disagree with it (K49).
func TestANegativeAmountIsRefused(t *testing.T) {
	_, err := bybit.NormalizeWithdrawal(context.Background(), &resolver{}, ingestContext(),
		json.RawMessage(`{"withdrawId":"x","coin":"USDT","amount":"-500","status":"success","updateTime":"1789999200000"}`))
	require.ErrorIs(t, err, bybit.ErrMalformed)
}

// The timestamp is epoch milliseconds in a string. Binance's own withdrawal times arrive as
// "2019-10-12 11:12:02" with no timezone stated, which is half of why they cannot be
// normalized at all -- so a value in that shape is refused here rather than coerced (B4, F5).
func TestANonEpochTimestampIsRefused(t *testing.T) {
	_, err := bybit.NormalizeWithdrawal(context.Background(), &resolver{}, ingestContext(),
		json.RawMessage(`{"withdrawId":"x","coin":"USDT","amount":"1","status":"success","updateTime":"2019-10-12 11:12:02"}`))
	require.ErrorIs(t, err, bybit.ErrMalformed)
}

// The page carries its cursor, and an empty one means the walk is done (B3).
func TestAPageCarriesItsCursor(t *testing.T) {
	deposits, err := bybit.DecodePage(fixture(t, "deposit_records.json"))
	require.NoError(t, err)
	require.NotEmpty(t, deposits.Cursor)

	withdrawals, err := bybit.DecodePage(fixture(t, "withdraw_records.json"))
	require.NoError(t, err)
	require.Empty(t, withdrawals.Cursor, "no cursor means there is no next page")
}

// The venue answers for under thirty days, so a wider window is refused BEFORE the request is
// sent. The alternative is an error from the venue that a walk could mistake for an empty
// page -- which is a gap recorded as a complete history, with the cursor moved past it (B3).
func TestAWindowWiderThanTheVenueAnswersForIsRefused(t *testing.T) {
	end := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

	require.NoError(t, bybit.HistoryQuery{
		Start: end.Add(-bybit.MaxHistoryWindow), End: end,
	}.Validate(), "the constant this package walks with must itself be acceptable")

	require.Error(t, bybit.HistoryQuery{
		Start: end.Add(-40 * 24 * time.Hour), End: end,
	}.Validate())

	require.Error(t, bybit.HistoryQuery{
		Start: end.Add(-30 * 24 * time.Hour), End: end,
	}.Validate(), "the venue says LESS than 30 days, so exactly 30 is not a window it answers")

	require.Error(t, bybit.HistoryQuery{Start: end, End: end.Add(-time.Hour)}.Validate(),
		"a window that ends before it starts")

	require.Error(t, bybit.HistoryQuery{
		Start: end.Add(-time.Hour), End: end, Limit: bybit.HistoryPageSize + 1,
	}.Validate(), "the venue pages at most fifty rows")
}
