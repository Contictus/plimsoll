// Package httpapi is the read-side HTTP surface: routing, the session cookie, and the
// handlers. It never writes ledger events and never places an order (L13,
// ARCHITECTURE.md section 10). The envelope every response carries lives in
// internal/freshness, which the packages that raise a reason also depend on.
package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/crypto"
	"github.com/Contictus/plimsoll/backend/internal/events"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/Contictus/plimsoll/backend/internal/valuation"
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

	// Keys seals the secrets that arrive through the API -- today, alert channel tokens
	// (K25). The API decrypts nothing: reading a credential is the worker's job, and a
	// process that cannot decrypt is a process a leaked session cannot make decrypt.
	Keys crypto.KeyProvider

	// Events is the live-update bus. Nil in a process that serves no streams -- and nil in a
	// test that does not need them, which is why it is an interface (K51).
	Events events.Subscriber

	// LeaseTTL is how long a worker's status report stays believable. It is the API's copy
	// of the worker's lease TTL: a report older than one lease came from a worker that no
	// longer holds the integration, and "live, as of forty minutes ago" is the sentence the
	// freshness object exists to keep out of a response (K39).
	LeaseTTL time.Duration

	// PriceTTL is how old the oldest price inside a valuation run may be before a response
	// says so. It is sized by how the prices are produced rather than by how fast a market
	// moves: the stream only reports tickers that changed (F7), so a quiet instrument's age
	// is bounded by the worker's REST re-snapshot and not by its own trading. Tripping this
	// therefore means the snapshot loop has stopped, which is worth a warning; a tighter
	// value would only report that some listed pair is quiet, which is not news.
	PriceTTL time.Duration

	// CollateralTTL is how old a captured margin picture may be before the response says
	// so. Sized by the worker's capture interval rather than by how fast a market moves: a
	// snapshot older than several captures means the capture loop has stopped, and that --
	// not volatility -- is what a reader needs to be told, because the number on the screen
	// then describes an account that has since moved.
	CollateralTTL time.Duration

	// PegAssets is the comma-separated peg configuration a rebuilt run terminates its price
	// paths with (K17). The API needs it because `?at=` rebuilds a run rather than reading
	// one; it is the same setting the worker produces runs with, and the resolution is
	// shared so the two processes cannot terminate a path differently.
	PegAssets string
}

// defaultLeaseTTL matches cmd/worker's. Duplicated rather than shared because the two
// processes are deployed separately and may briefly disagree; the consequence of a stale
// value here is a status believed a little too long or too briefly, never a wrong number.
const defaultLeaseTTL = 2 * time.Minute

// defaultCollateralTTL allows four of cmd/worker's captures to be missed before a reader is
// warned, so one slow response does not flag an account that is being watched perfectly well.
const defaultCollateralTTL = 2 * time.Minute

// defaultPriceTTL allows one re-snapshot interval (15 minutes, cmd/worker) plus the slack
// for a run to be produced and read.
const defaultPriceTTL = 20 * time.Minute

// NewRouter builds the HTTP surface. Every operation is behind requireSession unless it
// declares itself public, so the failure mode of forgetting to think about auth is a 401,
// not an open endpoint.
//
// The OpenAPI document and the docs page are served unauthenticated: they describe the
// contract and carry no tenant data.
func NewRouter(d Deps) http.Handler {
	if d.LeaseTTL <= 0 {
		d.LeaseTTL = defaultLeaseTTL
	}
	if d.PriceTTL <= 0 {
		d.PriceTTL = defaultPriceTTL
	}
	if d.CollateralTTL <= 0 {
		d.CollateralTTL = defaultCollateralTTL
	}
	if d.PegAssets == "" {
		d.PegAssets = valuation.DefaultPegAssets
	}
	router := chi.NewMux()
	api := humachi.New(router, huma.DefaultConfig("Plimsoll", APIVersion))
	api.UseMiddleware(d.requireSession(api))

	d.registerHealth(api)
	d.registerAuth(api)
	d.registerPortfolio(api)
	d.registerLineage(api)
	d.registerPnL(api)
	d.registerRisk(api)
	d.registerStrategy(api)
	d.registerExposure(api)
	d.registerAlerts(api)
	d.registerQuality(api)

	// Not a Huma operation: a response that never ends is not a value returned once (K51).
	d.registerStreams(router)

	return router
}
