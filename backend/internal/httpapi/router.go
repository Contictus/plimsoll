package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

// APIVersion is the version reported in the OpenAPI document. It tracks the API contract,
// not the build.
const APIVersion = "0.1.0"

// Database is the slice of the connection pool this package uses: a transaction for
// tenant data, and a ping for the health check. Taking an interface rather than
// *pgxpool.Pool is what lets the depguard rule keep a pool out of the request path (K15).
type Database interface {
	tenancy.Beginner
	Ping(ctx context.Context) error
}

// Deps is everything the router needs, supplied by cmd/api. Now is injected rather than
// called directly so session expiry is testable without sleeping (L4).
type Deps struct {
	DB   Database
	Auth *auth.Service
	Now  func() time.Time
}

// NewRouter builds the HTTP surface. Every operation is behind requireSession unless it
// declares itself public, so the failure mode of forgetting to think about auth is a 401,
// not an open endpoint.
//
// The OpenAPI document and the docs page are served unauthenticated: they describe the
// contract and carry no tenant data.
func NewRouter(d Deps) http.Handler {
	router := chi.NewMux()
	api := humachi.New(router, huma.DefaultConfig("Plimsoll", APIVersion))
	api.UseMiddleware(d.requireSession(api))

	d.registerHealth(api)
	d.registerAuth(api)

	return router
}
