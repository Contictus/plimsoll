-- name: NotifyAccount :exec
-- The live-update hint (K51). pg_notify rather than the NOTIFY statement because the channel
-- name is per account and NOTIFY cannot take a parameter -- and building an identifier by
-- string concatenation is how an injection gets written by someone in a hurry.
--
-- Delivered on COMMIT, which is the property that makes this correct: a subscriber is never
-- told to re-read a change that then rolls back.
SELECT pg_notify(sqlc.arg(channel), sqlc.arg(payload));
