-- name: UpsertFinding :exec
-- Opens a finding, or touches the one already open for this subject.
--
-- The conflict target is the partial unique index, which covers open rows only -- so a
-- problem that returns after closing does not collide with its closed predecessor and starts
-- a new incident instead (K53). opened_at is deliberately NOT updated: it is when the problem
-- started, and moving it would erase how long it has been true.
INSERT INTO findings (
  account_id, integration_id, kind, subject, severity, detail, delta, raw,
  opened_at, last_seen_at, occurrences
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, 1)
ON CONFLICT (account_id, integration_id, kind, subject) WHERE closed_at IS NULL
DO UPDATE SET
  last_seen_at = EXCLUDED.last_seen_at,
  occurrences  = findings.occurrences + 1,
  severity     = EXCLUDED.severity,
  detail       = EXCLUDED.detail,
  delta        = EXCLUDED.delta,
  raw          = EXCLUDED.raw;

-- name: CloseUnseenFindings :exec
-- Closes every open finding this pass owns and did not see.
--
-- The kind filter is the scope: a pass may only close what it was looking for, or a quiet
-- reconciliation run would clear a negative balance nobody has fixed. sqlc.arg(subjects) is
-- this pass's complete subject list; an empty pass closes everything it owns, which is the
-- correct reading of "I looked and found nothing".
UPDATE findings
   SET closed_at = @closed_at
 WHERE account_id     = @account_id
   AND integration_id = @integration_id
   AND closed_at IS NULL
   AND kind = ANY(@kinds::text[])
   AND NOT (subject = ANY(@subjects::text[]));

-- name: ListOpenFindings :many
-- What is wrong right now: worst first, then oldest first, because a problem that has been
-- true for a week is more interesting than one that started a minute ago.
SELECT id, integration_id, kind, subject, severity, detail, delta, raw,
       opened_at, last_seen_at, closed_at, occurrences
  FROM findings
 WHERE account_id = $1 AND closed_at IS NULL
 ORDER BY CASE severity WHEN 'error' THEN 0 WHEN 'warn' THEN 1 ELSE 2 END, opened_at;

-- name: ListFindingHistory :many
-- Open and closed alike, newest incident first. This is the evidence K55 requires before
-- automatic correction is ever allowed to act on a classification.
SELECT id, integration_id, kind, subject, severity, detail, delta, raw,
       opened_at, last_seen_at, closed_at, occurrences
  FROM findings
 WHERE account_id = $1
 ORDER BY opened_at DESC;
