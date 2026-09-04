package worker

import (
	"context"
	"fmt"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
)

// Assignment is one integration a worker may try to claim: whose it is, and which one.
type Assignment struct {
	AccountID     uuid.UUID
	IntegrationID uuid.UUID
}

// ActiveIntegrations lists every integration there is anything to run for, across accounts.
//
// It is the worker's only cross-account read and it goes through the SECURITY DEFINER
// surface in 00015 rather than through the integrations table -- FORCE ROW LEVEL SECURITY
// binds the owner too, so a function over that table would run as the owner and still see
// nothing. Everything the worker does afterwards is bound to one account and scoped by RLS
// like every other query (L12).
//
// db is the pool rather than a transaction: this read carries no account, so there is no
// account to bind.
func ActiveIntegrations(ctx context.Context, db store.DBTX) ([]Assignment, error) {
	rows, err := store.New(db).WorkerActiveIntegrations(ctx)
	if err != nil {
		return nil, fmt.Errorf("worker: list active integrations: %w", err)
	}
	out := make([]Assignment, 0, len(rows))
	for _, row := range rows {
		out = append(out, Assignment{
			AccountID:     row.AccountID,
			IntegrationID: row.IntegrationID,
		})
	}
	return out, nil
}
