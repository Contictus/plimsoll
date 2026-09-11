package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// transferBody is one joined movement, with both halves' own numbers.
//
// The fee is not a field: it is `out_quantity` minus `in_quantity`, and showing both is what
// lets a reader see it rather than be told it. A single "fee" figure would be our arithmetic
// presented as the venue's fact.
type transferBody struct {
	ID     uuid.UUID `json:"id"`
	Asset  string    `json:"asset"`
	Method string    `json:"method" enum:"txid,heuristic,manual" doc:"txid is proof; heuristic is a match on amount and time; manual is the user's own"`

	OutExchange  string    `json:"out_exchange"`
	OutQuantity  string    `json:"out_quantity"`
	OutEventTime time.Time `json:"out_event_time"`

	InExchange  string    `json:"in_exchange"`
	InQuantity  string    `json:"in_quantity"`
	InEventTime time.Time `json:"in_event_time"`

	LinkedAt time.Time `json:"linked_at"`
}

type transfersBody struct {
	Transfers []transferBody `json:"transfers"`
	freshness.Envelope
}

type linkInput struct {
	Body struct {
		// The two ledger rows, by seq. A user resolving the queue is pointing at events the
		// register named, so they are addressed the way the register names them.
		OutSeq int64 `json:"out_seq" required:"true" doc:"the withdrawal's ledger seq"`
		InSeq  int64 `json:"in_seq"  required:"true" doc:"the deposit's ledger seq"`
	}
}

type unlinkInput struct {
	ID uuid.UUID `path:"id"`
}

// errNoSuchLeg covers "no such event", "not a withdrawal", and "someone else's", deliberately
// without distinguishing them: telling a caller which of the three it was confirms another
// account's ledger by its row number (L12).
var errNoSuchLeg = errors.New("httpapi: no such transfer leg")

func (d Deps) registerTransfers(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-transfers",
		Method:      http.MethodGet,
		Path:        "/transfers",
		Summary:     "Movements between venues, joined",
		Description: "A link changes no number: the deposit already added and the withdrawal" +
			" already subtracted, on two different integrations, and both were right. What it" +
			" changes is the reading -- a withdrawal taken for a disposal invents a realized" +
			" loss. Legs with no other half are in GET /data-quality, not here.",
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body transfersBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}

		var rows []store.ListTransferLinksRow
		if err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			rows, err = q.ListTransferLinks(ctx, accountID)
			return err
		}); err != nil {
			return nil, huma.Error500InternalServerError("could not read transfers", err)
		}

		body := transfersBody{Transfers: make([]transferBody, 0, len(rows))}
		for _, r := range rows {
			out := transferBody{
				ID: r.ID, Asset: r.CanonicalSymbol, Method: r.Method,
				OutExchange: r.OutExchange, OutEventTime: r.OutEventTime,
				InExchange: r.InExchange, InEventTime: r.InEventTime,
				LinkedAt: r.LinkedAt,
			}
			if r.OutQuantity.Valid {
				out.OutQuantity = r.OutQuantity.Decimal.String()
			}
			if r.InQuantity.Valid {
				out.InQuantity = r.InQuantity.Decimal.String()
			}
			body.Transfers = append(body.Transfers, out)
		}
		body.Envelope = freshness.Envelope{
			AsOf: d.Now().UTC(), Freshness: freshness.New(),
		}
		return &struct{ Body transfersBody }{Body: body}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "link-transfer",
		Method:        http.MethodPost,
		Path:          "/transfers",
		Summary:       "Join two legs the matcher would not join",
		DefaultStatus: http.StatusCreated,
		Description: "The matcher refuses ambiguity rather than guessing (K57), so the queue" +
			" holds the cases a human can settle and it cannot. This writes the link and" +
			" nothing else: no ledger row is touched, and no balance changes.",
	}, func(ctx context.Context, in *linkInput) (*struct {
		Body struct {
			Linked bool `json:"linked"`
		}
	}, error,
	) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}

		err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			// Both halves are confirmed to be the caller's own AND to be the direction they
			// claim, before anything is written. Without the direction check a user could
			// join two deposits, which is a movement that did not happen.
			if err := confirmLeg(ctx, q, accountID, in.Body.OutSeq, ledger.TypeWithdrawal); err != nil {
				return err
			}
			if err := confirmLeg(ctx, q, accountID, in.Body.InSeq, ledger.TypeDeposit); err != nil {
				return err
			}
			return q.InsertTransferLink(ctx, store.InsertTransferLinkParams{
				AccountID: accountID,
				OutSeq:    in.Body.OutSeq,
				InSeq:     in.Body.InSeq,
				Method:    "manual",
			})
		})
		if errors.Is(err, errNoSuchLeg) {
			return nil, huma.Error404NotFound("no such transfer leg")
		}
		if err != nil {
			// A unique violation here is a leg already half of a transfer. 409 rather than
			// 500: the request was understood and the state refuses it.
			return nil, huma.Error409Conflict("one of those legs is already part of a transfer")
		}

		out := &struct {
			Body struct {
				Linked bool `json:"linked"`
			}
		}{}
		out.Body.Linked = true
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "unlink-transfer",
		Method:        http.MethodDelete,
		Path:          "/transfers/{id}",
		Summary:       "Undo a join",
		DefaultStatus: http.StatusNoContent,
		Description: "A link is an assertion about two events, not a record of what happened," +
			" so a user who joined the wrong two may unjoin them. The events are untouched" +
			" either way (L2).",
	}, func(ctx context.Context, in *unlinkInput) (*struct{}, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}

		var removed int64
		if err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			removed, err = q.DeleteTransferLink(ctx, store.DeleteTransferLinkParams{
				AccountID: accountID, ID: in.ID,
			})
			return err
		}); err != nil {
			return nil, huma.Error500InternalServerError("could not remove the link", err)
		}
		if removed == 0 {
			return nil, huma.Error404NotFound("no such transfer")
		}
		return &struct{}{}, nil
	})
}

func confirmLeg(
	ctx context.Context, q *store.Queries, accountID uuid.UUID, seq int64, want ledger.EventType,
) error {
	_, err := q.GetTransferLegForLinking(ctx, store.GetTransferLegForLinkingParams{
		AccountID: accountID,
		Seq:       seq,
		EventType: string(want),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoSuchLeg
	}
	return err
}
