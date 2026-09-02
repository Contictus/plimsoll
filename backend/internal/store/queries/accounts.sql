-- name: GetAccountByID :one
-- Scoped by id as the primary defence (L12); the RLS policy on accounts is the backstop
-- underneath it. Always called through tenancy.InTx.
SELECT id, email, is_admin, created_at, disabled_at
FROM accounts
WHERE id = $1;
