package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/freshness"
	"github.com/Contictus/plimsoll/backend/internal/quality"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// findingBody is one problem, as a reader sees it.
//
// Delta is a string like every other number (L1), and it is EMPTY rather than "0" where the
// finding has no magnitude: a problem we could not measure and a problem measured at zero are
// opposite claims, and rendering both as zero is how the second gets ignored (L11).
type findingBody struct {
	ID            uuid.UUID `json:"id"`
	IntegrationID uuid.UUID `json:"integration_id"`

	Kind     string `json:"kind"     doc:"missing_event, duplicate, rounding, unsupported, or one of the coherence checks"`
	Subject  string `json:"subject"  doc:"the asset or instrument the finding is about"`
	Severity string `json:"severity" enum:"info,warn,error"`
	Detail   string `json:"detail"`
	Delta    string `json:"delta"    doc:"the size of the disagreement; empty when the finding has no magnitude"`

	// OpenedAt is when the problem started and does not move while it persists; Occurrences
	// counts how many passes have seen it since. Together they answer "how long, and how
	// consistently", which one timestamp cannot (K53).
	OpenedAt    time.Time  `json:"opened_at"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	Occurrences int32      `json:"occurrences"`
}

type dataQualityBody struct {
	// Open is what is wrong right now, worst first. Closed findings are reachable through
	// `?history=true` and are never mixed in here: the register's front page is the present
	// tense, or a resolved problem reads as a live one.
	Open []findingBody `json:"open"`

	freshness.Envelope
}

func toFindingBody(s quality.Stored) findingBody {
	out := findingBody{
		ID:            s.ID,
		IntegrationID: s.IntegrationID,
		Kind:          s.Kind,
		Subject:       s.Subject,
		Severity:      s.Severity,
		Detail:        s.Detail,
		OpenedAt:      s.OpenedAt,
		LastSeenAt:    s.LastSeenAt,
		Occurrences:   s.Occurrences,
	}
	if s.Delta.Valid {
		out.Delta = s.Delta.Decimal.String()
	}
	if s.ClosedAt.Valid {
		closed := s.ClosedAt.Time
		out.ClosedAt = &closed
	}
	return out
}

type dataQualityInput struct {
	History bool `query:"history" doc:"include findings that have since closed"`
}

type resyncInput struct {
	ID uuid.UUID `path:"id"`
}

// errNoSuchIntegration covers both "no such integration" and "someone else's", deliberately
// without distinguishing them (L12).
var errNoSuchIntegration = errors.New("httpapi: no such integration")

// registerQuality mounts the register and the one action that answers it.
//
// GET /data-quality is the product's own claim, made checkable: "show me why your numbers are
// right" is the pitch, and an endpoint that lists everything we know to be wrong is the only
// honest answer to it. It returns 200 with an empty list when nothing is wrong -- an error
// status would make "we are fine" indistinguishable from "we could not tell you".
func (d Deps) registerQuality(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "data-quality",
		Method:      http.MethodGet,
		Path:        "/data-quality",
		Summary:     "Everything we know to be wrong, or cannot account for",
		Description: "A finding has a lifetime, not a timestamp: the same problem seen on" +
			" twelve consecutive passes is one open finding, not twelve records. `opened_at`" +
			" is when it started and does not move; `occurrences` counts the passes since.",
	}, func(ctx context.Context, in *dataQualityInput) (*struct{ Body dataQualityBody }, error) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}

		var rows []quality.Stored
		if err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			var err error
			if in.History {
				rows, err = quality.History(ctx, q, accountID)
			} else {
				rows, err = quality.Open(ctx, q, accountID)
			}
			return err
		}); err != nil {
			return nil, huma.Error500InternalServerError("could not read the register", err)
		}

		body := dataQualityBody{Open: make([]findingBody, 0, len(rows))}
		reasons := make([]freshness.Reason, 0, 1)
		worst := freshness.SeverityInfo
		for _, r := range rows {
			body.Open = append(body.Open, toFindingBody(r))
			if r.ClosedAt.Valid {
				continue
			}
			if r.Severity == quality.SeverityError {
				worst = freshness.SeverityError
			} else if r.Severity == quality.SeverityWarn && worst == freshness.SeverityInfo {
				worst = freshness.SeverityWarn
			}
		}
		// The register describing itself: an open finding is exactly the condition
		// reconciliation_mismatch names, so the endpoint that lists them is not exempt from
		// saying so (L11).
		if len(body.Open) > 0 && worst != freshness.SeverityInfo {
			reasons = append(reasons, freshness.Reason{
				Code:     freshness.ReasonReconciliationMismatch,
				Severity: worst,
				Detail:   "the register holds open findings",
				Since:    oldestOpen(rows),
			})
		}
		body.Envelope = freshness.Envelope{AsOf: d.Now().UTC(), Freshness: freshness.New(reasons...)}
		return &struct{ Body dataQualityBody }{Body: body}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "resync-integration",
		Method:        http.MethodPost,
		Path:          "/integrations/{id}/resync",
		Summary:       "Walk this integration's history again",
		DefaultStatus: http.StatusAccepted,
		Description: "Detect and report, never auto-correct (K55). Resync rewinds the" +
			" backfill cursors so the walk runs again; anything genuinely missing is appended" +
			" by the ordinary ingest path under the ordinary dedup key, and anything already" +
			" present is deduplicated away. It writes no correction and touches no ledger row.",
	}, func(ctx context.Context, in *resyncInput) (*struct {
		Body struct {
			ScopesReopened int64 `json:"scopes_reopened"`
		}
	}, error,
	) {
		accountID, ok := AccountFromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("unauthorized")
		}

		var reopened int64
		err := tenancy.InTx(ctx, d.DB, accountID, func(q *store.Queries) error {
			// Ownership is checked before anything is rewound, and a miss is 404 rather than
			// 403: telling a caller that an id exists but is not theirs confirms another
			// account's data by its identifier (L12).
			exists, err := q.IntegrationExists(ctx, store.IntegrationExistsParams{
				AccountID:     accountID,
				IntegrationID: in.ID,
			})
			if err != nil {
				return err
			}
			if !exists {
				// EXISTS answers false rather than raising, so the miss has to be turned into
				// one deliberately -- otherwise an unknown id rewinds nothing and reports
				// success, which reads to the caller as a resync that ran.
				return errNoSuchIntegration
			}
			reopened, err = q.ReopenBackfillScopes(ctx, store.ReopenBackfillScopesParams{
				AccountID:     accountID,
				IntegrationID: in.ID,
			})
			return err
		})
		// One answer for "it is not yours" and "it does not exist". Two different answers
		// would make the status code itself an oracle for another account's integration ids.
		if errors.Is(err, errNoSuchIntegration) || errors.Is(err, pgx.ErrNoRows) {
			return nil, huma.Error404NotFound("no such integration")
		}
		if err != nil {
			return nil, huma.Error500InternalServerError("could not reopen the walk", err)
		}

		out := &struct {
			Body struct {
				ScopesReopened int64 `json:"scopes_reopened"`
			}
		}{}
		out.Body.ScopesReopened = reopened
		return out, nil
	})
}

// oldestOpen is when the earliest still-open finding started. `since` on a freshness reason
// means "how long has this been true", and the newest one would understate it every time.
func oldestOpen(rows []quality.Stored) time.Time {
	var oldest time.Time
	for _, r := range rows {
		if r.ClosedAt.Valid {
			continue
		}
		if oldest.IsZero() || r.OpenedAt.Before(oldest) {
			oldest = r.OpenedAt
		}
	}
	return oldest
}
