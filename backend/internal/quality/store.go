package quality

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Record applies one producer's pass to the register, in the caller's transaction.
//
// The whole lifetime happens here: every finding in the pass is opened or touched, and every
// open finding the pass OWNS but did not see is closed. Doing both inside one transaction is
// what makes a crash mid-pass leave the register coherent rather than half-swept -- a register
// showing a problem as open when it was fixed, or fixed when it is open, is worse than no
// register at all.
//
// q must come from tenancy.InTx (L12).
func Record(
	ctx context.Context,
	q *store.Queries,
	accountID, integrationID uuid.UUID,
	p Pass,
) error {
	if len(p.Owns) == 0 {
		return fmt.Errorf("quality: a pass that owns no kinds cannot close anything")
	}

	subjects := make([]string, 0, len(p.Found))
	for _, f := range p.Found {
		if err := q.UpsertFinding(ctx, store.UpsertFindingParams{
			AccountID:     accountID,
			IntegrationID: integrationID,
			Kind:          f.Kind,
			Subject:       f.Subject,
			Severity:      f.Severity,
			Detail:        f.Detail,
			Delta:         f.Delta,
			Raw:           f.Raw,
			OpenedAt:      p.At.UTC(),
		}); err != nil {
			return fmt.Errorf("quality: record %s for %q: %w", f.Kind, f.Subject, err)
		}
		subjects = append(subjects, f.Subject)
	}

	closedAt := p.At.UTC()
	if err := q.CloseUnseenFindings(ctx, store.CloseUnseenFindingsParams{
		ClosedAt:      &closedAt,
		AccountID:     accountID,
		IntegrationID: integrationID,
		Kinds:         p.Owns,
		Subjects:      subjects,
	}); err != nil {
		return fmt.Errorf("quality: close unseen findings: %w", err)
	}
	return nil
}

// Open returns what is wrong right now, worst first. This is what the endpoint serves and what
// decides whether a response carries reconciliation_mismatch (L11).
func Open(ctx context.Context, q *store.Queries, accountID uuid.UUID) ([]Stored, error) {
	rows, err := q.ListOpenFindings(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("quality: list open findings: %w", err)
	}
	out := make([]Stored, 0, len(rows))
	for _, r := range rows {
		out = append(out, stored(r.ID, r.IntegrationID, r.Kind, r.Subject, r.Severity,
			r.Detail, r.Delta, r.Raw, r.OpenedAt, r.LastSeenAt, r.ClosedAt, r.Occurrences))
	}
	return out, nil
}

// History returns open and closed findings alike. A closed finding is not deleted, because the
// record of what was wrong and when it stopped is the evidence a classification is argued from
// (K55).
func History(ctx context.Context, q *store.Queries, accountID uuid.UUID) ([]Stored, error) {
	rows, err := q.ListFindingHistory(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("quality: list finding history: %w", err)
	}
	out := make([]Stored, 0, len(rows))
	for _, r := range rows {
		out = append(out, stored(r.ID, r.IntegrationID, r.Kind, r.Subject, r.Severity,
			r.Detail, r.Delta, r.Raw, r.OpenedAt, r.LastSeenAt, r.ClosedAt, r.Occurrences))
	}
	return out, nil
}

// stored maps one row onto the domain type. The two queries return structurally identical rows
// under different generated names, and the mapping is written once rather than twice so a new
// column cannot be carried by one path and dropped by the other.
func stored(
	id, integrationID uuid.UUID,
	kind, subject, severity, detail string,
	delta decimal.NullDecimal,
	raw []byte,
	openedAt, lastSeenAt time.Time,
	closedAt *time.Time,
	occurrences int32,
) Stored {
	out := Stored{
		ID:            id,
		IntegrationID: integrationID,
		Kind:          kind,
		Subject:       subject,
		Severity:      severity,
		Detail:        detail,
		Delta:         delta,
		Raw:           json.RawMessage(raw),
		OpenedAt:      openedAt.UTC(),
		LastSeenAt:    lastSeenAt.UTC(),
		Occurrences:   occurrences,
	}
	// A nil closing time means still open. Copying the zero time in would read as "closed in
	// year one" to anything that forgot to check Valid.
	if closedAt != nil {
		out.ClosedAt = ClosedAt{Time: closedAt.UTC(), Valid: true}
	}
	return out
}
