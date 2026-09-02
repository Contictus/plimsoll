-- The pre-authentication surface. Every query here goes through a SECURITY DEFINER
-- function because it runs before an account context exists and therefore cannot go
-- through tenancy.InTx. See migration 00003 for why that is, and for what each function
-- is allowed to see.

-- name: LookupCredentials :one
-- The function returns SETOF account_credentials, so sqlc types this row from the table
-- definition. Returns no rows for an unknown email, which the caller sees as
-- pgx.ErrNoRows and must not distinguish from a wrong password.
SELECT * FROM auth_lookup_credentials(sqlc.arg(email));

-- name: ResolveSession :one
SELECT auth_resolve_session(
  sqlc.arg(account_id), sqlc.arg(token_hash), sqlc.arg(now)
) AS account_id;

-- name: ConsumeInvite :one
SELECT auth_consume_invite(
  sqlc.arg(token_hash), sqlc.arg(email), sqlc.arg(password_hash), sqlc.arg(now)
) AS account_id;

-- name: CreateSession :exec
SELECT auth_create_session(
  sqlc.arg(account_id), sqlc.arg(token_hash), sqlc.arg(expires_at)
);

-- name: DeleteSession :exec
SELECT auth_delete_session(sqlc.arg(account_id), sqlc.arg(token_hash));

-- name: CreateInvite :exec
-- Reached only by plimsollctl, which connects as plimsoll_owner. The app role holds no
-- grant on invites, so this statement is unusable from the request path.
INSERT INTO invites (token_hash, email, expires_at)
VALUES (sqlc.arg(token_hash), sqlc.arg(email), sqlc.arg(expires_at));
