// Package asset resolves an exchange's symbol to a canonical asset, as of a point in
// time. It is the only path from an external symbol to an asset id: an exchange symbol is
// never a key and never resolved with today's mapping (K10, K22, L8).
package asset

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

// ErrUnknownSymbol means no alias window covers this symbol at this instant. It is
// deliberately not a fallback to the nearest window: a symbol recycled after a delisting
// would then attach a correct quantity to the wrong asset, and because the number looks
// plausible it gets debugged as a price problem for weeks (K22).
//
// Callers surface it as an unknown_symbol data-quality finding (K14) and mark the
// affected response degraded (K23, L11). They never guess.
var ErrUnknownSymbol = errors.New("asset: no alias covers this symbol at this time")

// Resolve returns the canonical asset behind an external symbol as it stood at `at`,
// which is always the event's own event_time -- never time.Now(). The exclusion
// constraint on asset_aliases guarantees at most one window matches, so this cannot
// silently pick between candidates.
func Resolve(
	ctx context.Context, q *store.Queries, source, externalSymbol string, at time.Time,
) (int64, error) {
	id, err := q.ResolveAssetAlias(ctx, store.ResolveAssetAliasParams{
		Source:         source,
		ExternalSymbol: externalSymbol,
		At:             at,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s %q at %s",
			ErrUnknownSymbol, source, externalSymbol, at.Format(time.RFC3339))
	}
	if err != nil {
		return 0, fmt.Errorf("asset: resolve %s %q: %w", source, externalSymbol, err)
	}
	return id, nil
}
