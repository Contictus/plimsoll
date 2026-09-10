package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/portfolio"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/strategy"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

type strategyBody struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind" enum:"directional,basis,market_neutral,other"`
	Positions int64     `json:"positions" doc:"how many positions are currently tagged into it"`
	CreatedAt time.Time `json:"created_at"`
}

type strategiesBody struct {
	Strategies []strategyBody `json:"strategies"`
}

type createStrategyInput struct {
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"80"`
		Kind string `json:"kind" enum:"directional,basis,market_neutral,other"`
	}
}

// assignStrategyInput carries a nullable id on purpose: untagging is a thing a user does, and
// a separate DELETE route would make "no strategy" a different operation from "this strategy"
// for a client that is only ever setting one field.
type assignStrategyInput struct {
	ID   string `path:"id" doc:"<integration_id>.<instrument_id>"`
	Body struct {
		StrategyID *uuid.UUID `json:"strategy_id" doc:"null clears the tag"`
	}
}

func (d Deps) registerStrategy(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-strategies",
		Method:      http.MethodGet,
		Path:        "/strategies",
		Summary:     "The groups this account has put its positions into",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body strategiesBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		var list []strategy.Strategy
		if err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			list, err = strategy.List(ctx, q, accountID)
			return err
		}); err != nil {
			return nil, err
		}

		out := make([]strategyBody, 0, len(list))
		for _, s := range list {
			out = append(out, strategyBody{
				ID: s.ID, Name: s.Name, Kind: s.Kind,
				Positions: s.Positions, CreatedAt: s.CreatedAt,
			})
		}
		return &struct{ Body strategiesBody }{Body: strategiesBody{Strategies: out}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "create-strategy",
		Method:      http.MethodPost,
		Path:        "/strategies",
		Summary:     "Name a group of positions",
		Description: "A delta-neutral pair reported ungrouped reads as leverage it does not" +
			" have, and the alerts that follow train the user to ignore all of them (K13).",
	}, func(ctx context.Context, in *createStrategyInput) (*struct {
		Body struct {
			ID uuid.UUID `json:"id"`
		}
	}, error,
	) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		var id uuid.UUID
		err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			id, err = strategy.Create(ctx, q, accountID, in.Body.Name, in.Body.Kind)
			return err
		})
		if errors.Is(err, strategy.ErrDuplicateName) {
			return nil, huma.Error409Conflict("this account already has a strategy with that name")
		}
		if err != nil {
			return nil, err
		}
		out := &struct {
			Body struct {
				ID uuid.UUID `json:"id"`
			}
		}{}
		out.Body.ID = id
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "assign-position-strategy",
		Method:      http.MethodPut,
		Path:        "/positions/{id}/strategy",
		Summary:     "Put one position into a group, or take it out of one",
	}, func(ctx context.Context, in *assignStrategyInput) (*struct{}, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}
		integrationID, instrumentID, err := portfolio.ParsePositionID(in.ID)
		if err != nil {
			return nil, huma.Error400BadRequest("malformed position id")
		}

		err = strategy.Assign(ctx, d.DB, accountID, integrationID, instrumentID, in.Body.StrategyID)
		switch {
		case errors.Is(err, strategy.ErrUnknownPosition):
			return nil, huma.Error404NotFound("no such position")
		case errors.Is(err, strategy.ErrUnknownStrategy):
			// Not 403. Distinguishing "someone else's" from "no such" would confirm
			// another account's data by its identifier.
			return nil, huma.Error404NotFound("no such strategy")
		case err != nil:
			return nil, err
		}
		return &struct{}{}, nil
	})
}
