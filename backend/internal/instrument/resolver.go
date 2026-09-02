// Package instrument resolves an exchange symbol to a canonical tradable instrument, as
// of a point in time. An asset is the thing (BTC); an instrument is the thing you trade
// (BTC-USDT-PERP). Keeping them apart is K10, and it is why spot and perpetual positions
// in the same coin do not merge.
package instrument

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

// Market is the venue's market segment. It is a typed parameter rather than a string so a
// caller cannot omit it or slide it into the wrong argument position -- which matters
// because omitting it is not a compile error in a string-typed API, and the resulting bug
// is two positions silently merged into one (K10).
type Market string

// The market segments V1 knows about. coinm is listed because the schema must be able to
// express it before M5, not because M1 ingests it.
const (
	MarketSpot  Market = "spot"
	MarketUSDM  Market = "usdm"
	MarketCoinM Market = "coinm"
)

// Kind is what the instrument is: a spot pair or a perpetual contract.
type Kind string

// The instrument kinds V1 supports.
const (
	KindSpot Kind = "spot"
	KindPerp Kind = "perp"
)

// ErrUnknownSymbol means no alias window covers this exchange, market and symbol at this
// instant. As in the asset registry, there is no fallback to the nearest window: a
// recycled symbol resolved with the wrong mapping produces a correct quantity attached to
// the wrong instrument (K22, L8). Callers raise an unknown_symbol finding (K14).
var ErrUnknownSymbol = errors.New("instrument: no alias covers this symbol at this time")

// Resolve returns the instrument behind an exchange symbol as it stood at `at`, which is
// always the event's own event_time -- never time.Now().
func Resolve(
	ctx context.Context,
	q *store.Queries,
	exchange string,
	market Market,
	exchangeSymbol string,
	at time.Time,
) (int64, error) {
	id, err := q.ResolveInstrumentAlias(ctx, store.ResolveInstrumentAliasParams{
		Exchange:       exchange,
		Market:         string(market),
		ExchangeSymbol: exchangeSymbol,
		At:             at,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s %s %q at %s",
			ErrUnknownSymbol, exchange, market, exchangeSymbol, at.Format(time.RFC3339))
	}
	if err != nil {
		return 0, fmt.Errorf("instrument: resolve %s %s %q: %w",
			exchange, market, exchangeSymbol, err)
	}
	return id, nil
}
