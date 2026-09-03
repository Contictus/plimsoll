-- +goose Up

-- Issue #4. The projector advances a keyset cursor and reads only events after it, but the
-- ledger accepts inserts in any order -- which is M2's exact topology: the stream ingests
-- live trades and moves the cursor to today, then the backfill appends last year. Those
-- rows sort below the cursor, are never read, and the position stays permanently wrong
-- with nothing in freshness to say so.
--
-- Detection is by row count rather than by a seq watermark. seq is assigned before commit,
-- so a reader can pass a value that is still in flight (K20, L6) -- a watermark would miss
-- exactly the case it exists for. A count of the events at or below the cursor is exact
-- under any commit interleaving: if it disagrees with what the projector folded, something
-- landed behind it, and the projection is rebuilt.
--
-- Existing rows default to 0 and so disagree on their first run. That is correct: they
-- were built before this check existed, and one rebuild is what makes them trustworthy.
ALTER TABLE projection_cursors
  ADD COLUMN projected_count BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE projection_cursors DROP COLUMN projected_count;
