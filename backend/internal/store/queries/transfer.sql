-- name: ListUnlinkedTransferLegs :many
-- Every withdrawal and deposit this account holds that is not already half of a transfer.
--
-- Both directions in one query, tagged by event_type, because the matcher needs them together
-- and two queries would be two instants -- a leg linked between them would be offered twice.
--
-- The NOT EXISTS is what makes a re-run idempotent: a leg already joined is not a candidate,
-- so matching twice produces the same links rather than a duplicate-key error.
SELECT e.seq, e.integration_id, i.exchange, a.canonical_symbol, e.quantity,
       e.event_type, e.event_time, e.raw
  FROM ledger_events e
  JOIN integrations i ON i.id = e.integration_id
  JOIN assets a ON a.id = e.asset_id
 WHERE e.account_id = sqlc.arg(account_id)
   AND e.event_type IN ('DEPOSIT', 'WITHDRAWAL')
   AND e.asset_id IS NOT NULL
   AND NOT EXISTS (
         SELECT 1 FROM transfer_links l
          WHERE l.out_seq = e.seq OR l.in_seq = e.seq)
 ORDER BY e.event_time, e.seq;

-- name: InsertTransferLink :exec
-- Joins two legs. The unique constraints on out_seq and in_seq mean a second attempt to claim
-- either half fails rather than quietly winning, which is the whole point of them.
INSERT INTO transfer_links (account_id, out_seq, in_seq, method)
VALUES (sqlc.arg(account_id), sqlc.arg(out_seq), sqlc.arg(in_seq), sqlc.arg(method));

-- name: ListTransferLinks :many
-- Every joined movement, newest first, with both halves' own numbers so a reader can see the
-- network fee as the difference rather than being told it.
SELECT l.id, l.method, l.linked_at,
       o.seq AS out_seq, oi.exchange AS out_exchange, o.quantity AS out_quantity,
       o.event_time AS out_event_time,
       d.seq AS in_seq,  di.exchange AS in_exchange,  d.quantity AS in_quantity,
       d.event_time AS in_event_time,
       a.canonical_symbol
  FROM transfer_links l
  JOIN ledger_events o ON o.seq = l.out_seq
  JOIN ledger_events d ON d.seq = l.in_seq
  JOIN integrations oi ON oi.id = o.integration_id
  JOIN integrations di ON di.id = d.integration_id
  JOIN assets a ON a.id = o.asset_id
 WHERE l.account_id = sqlc.arg(account_id)
 ORDER BY l.linked_at DESC;

-- name: DeleteTransferLink :execrows
-- Unjoins two legs. Allowed, unlike deleting a finding: a link is an assertion about two
-- events, not a record of what happened, and a user who joined the wrong two must be able to
-- say so. The events themselves are untouched (L2).
DELETE FROM transfer_links
 WHERE account_id = sqlc.arg(account_id) AND id = sqlc.arg(id);

-- name: GetTransferLegForLinking :one
-- One leg, confirmed to belong to the caller and to be the direction they claim. The manual
-- endpoint reads both halves through this before writing, so a user cannot join an event that
-- is not theirs -- RLS would already return nothing, and this turns that nothing into a 404
-- rather than a foreign key error the caller has to interpret.
SELECT e.seq, e.integration_id, e.event_type, e.asset_id, e.quantity, e.event_time
  FROM ledger_events e
 WHERE e.account_id = sqlc.arg(account_id)
   AND e.seq = sqlc.arg(seq)
   AND e.event_type = sqlc.arg(event_type);
