//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/httpapi"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const testPassword = "hunter2-hunter2"

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		DB:   pool,
		Auth: auth.NewService(store.New(pool), pool, 24*time.Hour),
		Now:  time.Now,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mintInvite writes an invite as the owner, the way plimsollctl does. The app role holds
// no grant on invites, so this cannot go through the API.
func mintInvite(t *testing.T, email string) string {
	t.Helper()
	ctx := context.Background()
	owner, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	defer owner.Close()

	plain, hash, err := auth.NewOpaqueToken()
	require.NoError(t, err)
	require.NoError(t, store.New(owner).CreateInvite(ctx, store.CreateInviteParams{
		TokenHash: hash,
		Email:     email,
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
	}))
	return plain
}

func uniqueEmail(prefix string) string { return prefix + "-" + uuid.NewString() + "@example.test" }

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body)) //nolint:noctx // httptest
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func sessionCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == httpapi.SessionCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in response", httpapi.SessionCookieName)
	return nil
}

// register accepts an invite and returns the session cookie it was issued.
func register(t *testing.T, srv *httptest.Server, email string) *http.Cookie {
	t.Helper()
	resp := postJSON(t, srv.URL+"/auth/accept-invite",
		`{"invite_token":"`+mintInvite(t, email)+`","email":"`+email+
			`","password":"`+testPassword+`"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return sessionCookie(t, resp)
}

func do(t *testing.T, method, url string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	require.NoError(t, err)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestHealthzReportsDatabaseReachable(t *testing.T) {
	resp := do(t, http.MethodGet, newServer(t).URL+"/healthz", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Status   string `json:"status"`
		Database string `json:"database"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, "ok", body.Status)
	require.Equal(t, "ok", body.Database)
}

func TestMeRequiresASession(t *testing.T) {
	resp := do(t, http.MethodGet, newServer(t).URL+"/me", nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// The session cookie must be inaccessible to JavaScript. Under K27 the API and the UI
// share one origin, so an HttpOnly cookie is the whole storage strategy -- there is no
// token in JS to steal.
func TestAcceptInviteSetsHardenedCookieAndMeWorks(t *testing.T) {
	srv := newServer(t)
	email := uniqueEmail("api")
	cookie := register(t, srv, email)

	require.True(t, cookie.HttpOnly, "session cookie must be HttpOnly")
	require.True(t, cookie.Secure, "session cookie must be Secure")
	require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	require.Equal(t, "/", cookie.Path)

	resp := do(t, http.MethodGet, srv.URL+"/me", cookie)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var me struct {
		AccountID string `json:"account_id"`
		Email     string `json:"email"`
		AsOf      string `json:"as_of"`
		Freshness struct {
			Status  string           `json:"status"`
			Reasons []map[string]any `json:"reasons"`
		} `json:"freshness"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&me))
	require.Equal(t, email, me.Email)
	require.NotEmpty(t, me.AccountID)

	// L10/L11: no data response leaves without as_of and freshness.
	require.NotEmpty(t, me.AsOf)
	require.Equal(t, "ok", me.Freshness.Status)
	require.NotNil(t, me.Freshness.Reasons)
}

func TestLoginIssuesAFreshSession(t *testing.T) {
	srv := newServer(t)
	email := uniqueEmail("login")
	first := register(t, srv, email)

	resp := postJSON(t, srv.URL+"/auth/login",
		`{"email":"`+email+`","password":"`+testPassword+`"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	second := sessionCookie(t, resp)
	require.NotEqual(t, first.Value, second.Value, "each login must mint a fresh session")

	me := do(t, http.MethodGet, srv.URL+"/me", second)
	require.Equal(t, http.StatusOK, me.StatusCode)
}

// L13: a failed login must not echo the submitted password anywhere in the response.
func TestFailedLoginLeaksNothing(t *testing.T) {
	const password = "distinctive-wrong-password"
	resp := postJSON(t, newServer(t).URL+"/auth/login",
		`{"email":"nobody@example.test","password":"`+password+`"}`)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NotContains(t, string(raw), password)
}

// A wrong password and an unknown email must be indistinguishable from the outside --
// same status, same body -- or login becomes a user-enumeration oracle.
func TestWrongPasswordLooksIdenticalToUnknownEmail(t *testing.T) {
	srv := newServer(t)
	email := uniqueEmail("oracle")
	register(t, srv, email)

	wrongPassword := postJSON(t, srv.URL+"/auth/login",
		`{"email":"`+email+`","password":"definitely-not-it"}`)
	unknownEmail := postJSON(t, srv.URL+"/auth/login",
		`{"email":"`+uniqueEmail("ghost")+`","password":"definitely-not-it"}`)

	require.Equal(t, wrongPassword.StatusCode, unknownEmail.StatusCode)

	a, err := io.ReadAll(wrongPassword.Body)
	require.NoError(t, err)
	b, err := io.ReadAll(unknownEmail.Body)
	require.NoError(t, err)
	require.Equal(t, string(a), string(b))
}

// K16: logout revokes server-side, immediately. The cleared cookie is a convenience; the
// property that matters is that the token no longer resolves.
func TestLogoutRevokesTheSessionAndClearsTheCookie(t *testing.T) {
	srv := newServer(t)
	cookie := register(t, srv, uniqueEmail("logout"))

	resp := do(t, http.MethodPost, srv.URL+"/auth/logout", cookie)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	cleared := sessionCookie(t, resp)
	require.Empty(t, cleared.Value)
	require.Negative(t, cleared.MaxAge, "the cleared cookie must expire the browser copy")

	replay := do(t, http.MethodGet, srv.URL+"/me", cookie)
	require.Equal(t, http.StatusUnauthorized, replay.StatusCode,
		"the revoked token must not resolve even when replayed")
}

func TestGarbageSessionCookieIsRejected(t *testing.T) {
	srv := newServer(t)
	for _, value := range []string{"", "garbage", "no-dot-here", "!!!.xyz"} {
		resp := do(t, http.MethodGet, srv.URL+"/me",
			&http.Cookie{Name: httpapi.SessionCookieName, Value: value})
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "cookie %q", value)
	}
}

// Authentication is default-deny: an operation is public only by saying so. This walks
// the OpenAPI document the router actually serves, so an endpoint added later without a
// thought about auth fails here rather than shipping open.
func TestEveryRouteOutsideThePublicSetRequiresASession(t *testing.T) {
	public := map[string]bool{
		"GET /healthz":             true,
		"POST /auth/login":         true,
		"POST /auth/accept-invite": true,
		"POST /auth/logout":        true,
	}

	srv := newServer(t)
	spec := do(t, http.MethodGet, srv.URL+"/openapi.json", nil)
	require.Equal(t, http.StatusOK, spec.StatusCode)

	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	require.NoError(t, json.NewDecoder(spec.Body).Decode(&doc))
	require.NotEmpty(t, doc.Paths, "the router registered no operations")

	checked := 0
	for path, methods := range doc.Paths {
		for method := range methods {
			route := strings.ToUpper(method) + " " + path
			if public[route] {
				continue
			}
			resp := do(t, strings.ToUpper(method), srv.URL+path, nil)
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
				"%s answered without a session; put it in the public set deliberately "+
					"or leave it behind requireSession", route)
			checked++
		}
	}
	require.Positive(t, checked, "no protected route was exercised")
}
