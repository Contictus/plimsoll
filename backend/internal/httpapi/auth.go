package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

// accountBody is what the caller learns about itself. It carries the Envelope for the
// same reason a portfolio response does: an endpoint that is allowed to answer without
// as_of and freshness is the precedent that lets the next one skip them too (L10, L11).
type accountBody struct {
	Envelope
	AccountID uuid.UUID `json:"account_id"`
	Email     string    `json:"email"`
	IsAdmin   bool      `json:"is_admin"`
}

// sessionOutput carries a freshly issued session. Huma writes the Set-Cookie header from
// the struct field, so the cookie flags live in one place (middleware.go) rather than
// being re-specified per endpoint.
type sessionOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      accountBody
}

type loginInput struct {
	Body struct {
		Email    string `json:"email"    format:"email" required:"true"`
		Password string `json:"password" minLength:"12" required:"true"`
	}
}

type acceptInviteInput struct {
	Body struct {
		InviteToken string `json:"invite_token" required:"true"`
		Email       string `json:"email"        format:"email" required:"true"`
		Password    string `json:"password"     minLength:"12" required:"true"`
	}
}

type meOutput struct {
	Body accountBody
}

// logoutInput reads the session cookie as an ordinary input rather than reaching for the
// request: the cookie is optional, so logging out with an already-dead session succeeds
// instead of erroring.
type logoutInput struct {
	Session http.Cookie `cookie:"plimsoll_session"`
}

type logoutOutput struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
}

// registerAuth wires the session lifecycle. login, accept-invite and logout are public by
// necessity -- they are how a session comes to exist, or stops existing -- and each one is
// listed in the API integration test's public set, so widening that set is a deliberate
// edit rather than an accident.
func (d Deps) registerAuth(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/auth/login",
		Summary:     "Exchange an email and password for a session cookie",
		Metadata:    public(),
	}, func(ctx context.Context, in *loginInput) (*sessionOutput, error) {
		now := d.Now()
		token, accountID, err := d.Auth.Login(ctx, in.Body.Email, in.Body.Password, now)
		if err != nil {
			return nil, d.authFailure(err)
		}
		return d.issue(ctx, token, accountID, now)
	})

	huma.Register(api, huma.Operation{
		OperationID: "accept-invite",
		Method:      http.MethodPost,
		Path:        "/auth/accept-invite",
		Summary:     "Consume an invite, create the account, and start a session",
		Metadata:    public(),
	}, func(ctx context.Context, in *acceptInviteInput) (*sessionOutput, error) {
		now := d.Now()
		token, accountID, err := d.Auth.AcceptInvite(
			ctx, in.Body.InviteToken, in.Body.Email, in.Body.Password, now)
		if err != nil {
			return nil, d.authFailure(err)
		}
		return d.issue(ctx, token, accountID, now)
	})

	huma.Register(api, huma.Operation{
		OperationID:   "logout",
		Method:        http.MethodPost,
		Path:          "/auth/logout",
		Summary:       "Revoke the current session",
		DefaultStatus: http.StatusNoContent,
		// Public because it acts on the cookie it is given, not on an authenticated
		// identity: a session that has already expired must still be logoutable, and
		// revoking a token requires holding it.
		Metadata: public(),
	}, func(ctx context.Context, in *logoutInput) (*logoutOutput, error) {
		if err := d.Auth.Logout(ctx, in.Session.Value); err != nil {
			return nil, err
		}
		return &logoutOutput{SetCookie: clearedSessionCookie()}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "me",
		Method:      http.MethodGet,
		Path:        "/me",
		Summary:     "The authenticated account",
	}, func(ctx context.Context, _ *struct{}) (*meOutput, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			// Unreachable while requireSession runs. Deliberately not a 401: answering
			// "unauthorized" here would let a route that lost its middleware look
			// correctly protected, and the test that walks every route would pass over
			// it. A 500 says what actually happened -- the server is misconfigured.
			return nil, huma.Error500InternalServerError("missing account context")
		}
		body, err := d.account(ctx, accountID)
		if err != nil {
			return nil, err
		}
		return &meOutput{Body: body}, nil
	})
}

// issue turns a freshly minted token into the session response, reading the account back
// so login and accept-invite answer with exactly the shape /me does.
func (d Deps) issue(
	ctx context.Context, token string, accountID uuid.UUID, now time.Time,
) (*sessionOutput, error) {
	body, err := d.account(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return &sessionOutput{
		SetCookie: sessionCookie(token, now.Add(d.Auth.SessionTTL())),
		Body:      body,
	}, nil
}

// account reads the caller's own row. It goes through tenancy.InTx like every other
// tenant read, so the RLS policy applies here too -- there is no privileged path for
// "just my own account" (L12).
func (d Deps) account(ctx context.Context, accountID uuid.UUID) (accountBody, error) {
	var row store.Account
	err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
		var err error
		row, err = q.GetAccountByID(ctx, accountID)
		return err
	})
	if err != nil {
		return accountBody{}, err
	}
	return accountBody{
		Envelope: Envelope{
			// The account is read inside the request, so the response is current as of
			// now. Once a valuation run backs a response, as_of comes from the run (L10).
			AsOf:      d.Now(),
			Freshness: NewFreshness(),
		},
		AccountID: row.ID,
		Email:     row.Email,
		IsAdmin:   row.IsAdmin,
	}, nil
}

// authFailure maps every credential and invite refusal onto one 401 with one message.
// Distinguishing "no such account" from "wrong password" from "invite already used" would
// turn these endpoints into oracles. Anything else is a real fault and is returned as-is,
// so it becomes a 500 and reaches the logs.
func (d Deps) authFailure(err error) error {
	if errors.Is(err, auth.ErrInvalidCredentials) || errors.Is(err, auth.ErrInvalidInvite) {
		return huma.Error401Unauthorized("unauthorized")
	}
	return err
}
