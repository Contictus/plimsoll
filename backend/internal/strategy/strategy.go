// Package strategy is how the user says two positions belong together.
//
// It is I/O around user input, not an engine: nothing here folds anything. The reason it is
// its own package is K30 -- a strategy assignment is something a human typed, and `positions`
// is a projection that every rebuild drops. A tag stored on the projection is erased by the
// rebuild, and the rebuild-equality test still passes, because both sides are equally empty.
package strategy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
)

// The closed vocabulary of strategy kinds, matching the schema's CHECK.
const (
	KindDirectional   = "directional"
	KindBasis         = "basis"
	KindMarketNeutral = "market_neutral"
	KindOther         = "other"
)

var (
	// ErrDuplicateName means this account already has a strategy by that name.
	ErrDuplicateName = errors.New("strategy: this account already has a strategy with that name")

	// ErrUnknownStrategy covers both "no such strategy" and "someone else's strategy", and
	// deliberately does not distinguish them. Telling a caller that an id exists but is not
	// theirs is itself a leak: it confirms another account's data by its identifier.
	ErrUnknownStrategy = errors.New("strategy: no such strategy")

	// ErrUnknownPosition means there is nothing to tag.
	ErrUnknownPosition = errors.New("strategy: no such position")
)

// PositionKey is the projection's own key, which is what a tag is keyed by.
type PositionKey struct {
	IntegrationID uuid.UUID
	InstrumentID  int64
}

// Strategy is one named group of positions.
type Strategy struct {
	ID        uuid.UUID
	Name      string
	Kind      string
	CreatedAt time.Time
	Positions int64
}

// Create registers a strategy for one account. q must come from tenancy.InTx (L12).
func Create(
	ctx context.Context, q *store.Queries, accountID uuid.UUID, name, kind string,
) (uuid.UUID, error) {
	if kind == "" {
		kind = KindDirectional
	}
	id, err := q.CreateStrategy(ctx, store.CreateStrategyParams{
		AccountID: accountID, Name: name, Kind: kind,
	})
	if isCode(err, uniqueViolation) {
		return uuid.Nil, ErrDuplicateName
	}
	if err != nil {
		// The name is the caller's own input and safe to echo; nothing else here is.
		return uuid.Nil, fmt.Errorf("strategy: create %q for %s: %w", name, accountID, err)
	}
	return id, nil
}

// List is every strategy this account has, with how many positions each one holds.
func List(ctx context.Context, q *store.Queries, accountID uuid.UUID) ([]Strategy, error) {
	rows, err := q.ListStrategies(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("strategy: list for %s: %w", accountID, err)
	}
	out := make([]Strategy, 0, len(rows))
	for _, r := range rows {
		out = append(out, Strategy{
			ID: r.ID, Name: r.Name, Kind: r.Kind,
			CreatedAt: r.CreatedAt, Positions: r.Positions,
		})
	}
	return out, nil
}

// Assign tags one position, or removes the tag when strategyID is nil.
//
// Removing is a DELETE rather than a null strategy_id, so "never tagged" and "untagged" are
// one state instead of two that every reader would have to remember to treat alike.
func Assign(
	ctx context.Context, db tenancy.Beginner, accountID, integrationID uuid.UUID,
	instrumentID int64, strategyID *uuid.UUID,
) error {
	return tenancy.InTx(ctx, db, accountID, func(q *store.Queries) error {
		exists, err := q.PositionExists(ctx, store.PositionExistsParams{
			AccountID: accountID, IntegrationID: integrationID, InstrumentID: instrumentID,
		})
		if err != nil {
			return fmt.Errorf("strategy: look up position %s/%d: %w",
				integrationID, instrumentID, err)
		}
		if !exists {
			return ErrUnknownPosition
		}
		if strategyID == nil {
			if err := q.ClearPositionStrategy(ctx, store.ClearPositionStrategyParams{
				AccountID: accountID, IntegrationID: integrationID, InstrumentID: instrumentID,
			}); err != nil {
				return fmt.Errorf("strategy: clear tag on %s/%d: %w",
					integrationID, instrumentID, err)
			}
			return nil
		}
		err = q.AssignPositionStrategy(ctx, store.AssignPositionStrategyParams{
			AccountID:     accountID,
			IntegrationID: integrationID,
			InstrumentID:  instrumentID,
			StrategyID:    *strategyID,
		})
		// The composite foreign key is what makes another account's strategy unreachable
		// here, so this is the storage layer answering rather than a check that could be
		// forgotten (K29's shape). Reading every foreign-key violation as an unknown
		// strategy is exact only because the position was proven to exist above -- which
		// proves its integration exists too, so the strategy is the one key left to break.
		if isCode(err, foreignKeyViolation) {
			return ErrUnknownStrategy
		}
		if err != nil {
			return fmt.Errorf("strategy: tag %s/%d with %s: %w",
				integrationID, instrumentID, *strategyID, err)
		}
		return nil
	})
}

// The two Postgres error codes this package turns into answers rather than 500s.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
)

func isCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// Of is every tagged position for one account, keyed the way the projection keys a position.
//
// A map rather than a slice because every caller is asking the same question one position at
// a time -- "which group is this in" -- while aggregating, and a linear scan per position
// would make the risk report quadratic in an account's own size.
func Of(
	ctx context.Context, q *store.Queries, accountID uuid.UUID,
) (map[PositionKey]Strategy, error) {
	rows, err := q.ListPositionStrategies(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("strategy: read tags for %s: %w", accountID, err)
	}
	out := make(map[PositionKey]Strategy, len(rows))
	for _, r := range rows {
		out[PositionKey{IntegrationID: r.IntegrationID, InstrumentID: r.InstrumentID}] =
			Strategy{ID: r.ID, Name: r.Name, Kind: r.Kind}
	}
	return out, nil
}
