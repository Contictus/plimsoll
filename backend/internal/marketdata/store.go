package marketdata

import (
	"context"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/shopspring/decimal"
)

// Tick is one price, filed under the minute it belongs to and ready to be stored.
type Tick struct {
	InstrumentID int64
	Minute       time.Time
	Price        decimal.Decimal
	Source       string

	// ObservedAt is when the venue said it, which is not the minute it is filed under and
	// not when we read the socket. It is what price_stale is measured from (L11), and it is
	// what decides which of two claims about the same minute wins.
	ObservedAt time.Time
}

// TickOf files a quote under its minute. Pure, so the filing rule is testable without a
// database and identical for the live recorder and the historical backfill.
func TickOf(instrumentID int64, q Quote, source string) Tick {
	return Tick{
		InstrumentID: instrumentID,
		Minute:       Minute(q.ObservedAt),
		Price:        q.Price,
		Source:       source,
		ObservedAt:   q.ObservedAt,
	}
}

// Writer stores ticks.
//
// It takes a plain handle rather than a transaction bound to an account, because prices are
// reference data: no account_id, no RLS, and no tenant to bind (00018). Forcing a tenancy
// wrapper here would ask a question the row has no answer to.
type Writer struct {
	DB store.DBTX
}

// Record stores every tick, letting the newest observation of a minute win.
//
// One statement per tick rather than a batch: the volume is one row per instrument per
// minute, which is small, and the upsert's guard has to be evaluated per row anyway. When
// the historical backfill makes this the hot path, this is where the copy goes.
func (w Writer) Record(ctx context.Context, ticks []Tick) error {
	q := store.New(w.DB)
	for _, t := range ticks {
		if err := q.UpsertPriceTick(ctx, store.UpsertPriceTickParams{
			InstrumentID: t.InstrumentID,
			Ts:           t.Minute,
			Price:        t.Price,
			Source:       t.Source,
			ObservedAt:   t.ObservedAt,
		}); err != nil {
			return fmt.Errorf("marketdata: record %s for instrument %d at %s: %w",
				t.Price, t.InstrumentID, t.Minute.Format(time.RFC3339), err)
		}
	}
	return nil
}
