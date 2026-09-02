package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// healthBody is deliberately not wrapped in an Envelope. /healthz is an operational probe
// read by compose and by the load balancer, not a data response -- there is no as_of
// because there is no valuation behind it.
type healthBody struct {
	Status   string `json:"status"   doc:"ok when the process can serve requests"`
	Database string `json:"database" doc:"ok, or unreachable"`
}

type healthOutput struct {
	Status int
	Body   healthBody
}

// registerHealth wires GET /healthz. It reports 503 rather than 200 when Postgres is
// unreachable: a health check that stays green while the database is down is worse than
// no health check, because it removes the one signal an operator has (L11).
func (d Deps) registerHealth(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "healthz",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Liveness and database reachability",
		Metadata:    public(),
	}, func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
		if err := d.DB.Ping(ctx); err != nil {
			// The error is not echoed: a DSN-shaped pgx error can carry the host and the
			// user (L13). The operator reads the reason from the logs.
			return &healthOutput{
				Status: http.StatusServiceUnavailable,
				Body:   healthBody{Status: "degraded", Database: "unreachable"},
			}, nil
		}
		return &healthOutput{
			Status: http.StatusOK,
			Body:   healthBody{Status: "ok", Database: "ok"},
		}, nil
	})
}
