package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

// SessionCookieName is the only place the session token is stored client-side. Under K27
// the UI and the API share one origin, so there is no token in JavaScript to steal and no
// CORS configuration to get wrong.
const SessionCookieName = "plimsoll_session"

// publicMetadataKey marks an operation that runs without a session. Authentication is
// default-deny: an operation is reachable unauthenticated only by carrying this flag, so
// an endpoint added without a thought about auth is protected rather than open.
const publicMetadataKey = "plimsoll:public"

// public is the metadata an operation attaches to opt out of requireSession. Written as a
// constructor so each operation gets its own map and cannot mutate a shared one.
func public() map[string]any { return map[string]any{publicMetadataKey: true} }

// accountCtxKey is unexported so no package outside this one can plant an account id in a
// context. The middleware is the only writer; AccountFromContext is the only reader.
type accountCtxKey struct{}

// AccountFromContext returns the authenticated account. Handlers pass it straight to
// tenancy.InTx; nothing else in the request path may derive an account id.
func AccountFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(accountCtxKey{}).(uuid.UUID)
	return id, ok
}

// requireSession resolves the session cookie into an account and binds it to the request
// context. Every refusal is the same 401 with the same body: a caller must not be able to
// tell a missing cookie from a forged one from an expired one.
func (d Deps) requireSession(api huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		if isPublic, _ := ctx.Operation().Metadata[publicMetadataKey].(bool); isPublic {
			next(ctx)
			return
		}

		cookie, err := huma.ReadCookie(ctx, SessionCookieName)
		if err != nil {
			d.unauthorized(api, ctx)
			return
		}
		accountID, err := d.Auth.ResolveSession(ctx.Context(), cookie.Value, d.Now())
		if err != nil {
			d.unauthorized(api, ctx)
			return
		}
		next(huma.WithValue(ctx, accountCtxKey{}, accountID))
	}
}

// unauthorized writes the single refusal this API gives for any authentication failure.
// The write error is discarded on purpose: the response has already begun, so there is
// nothing left to tell the client.
func (d Deps) unauthorized(api huma.API, ctx huma.Context) {
	_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "unauthorized")
}

// sessionCookie builds the cookie carrying a freshly issued session. Secure is set
// unconditionally: Caddy terminates TLS in front of the API (K27), and a cookie that is
// only sometimes Secure is a cookie that leaks on the one deployment that forgot.
func sessionCookie(token string, expires time.Time) http.Cookie {
	return http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// clearedSessionCookie expires the browser's copy on logout. It is a convenience only:
// the session is already dead server-side by the time this is sent (K16).
func clearedSessionCookie() http.Cookie {
	return http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}
