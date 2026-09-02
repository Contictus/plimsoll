//go:build integration

package auth_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), os.Getenv("PLIMSOLL_APP_DSN"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func newService(t *testing.T) *auth.Service {
	t.Helper()
	pool := appPool(t)
	return auth.NewService(store.New(pool), pool, 24*time.Hour)
}

// mintInvite writes an invite as the owner, the way plimsollctl does. The app role holds
// no grant on invites, so this cannot be done through the service.
func mintInvite(t *testing.T, email string) string {
	t.Helper()
	ctx := context.Background()
	owner, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	defer owner.Close()

	plain, hash, err := auth.NewOpaqueToken()
	require.NoError(t, err)
	require.NoError(t, store.New(owner).CreateInvite(ctx, store.CreateInviteParams{
		TokenHash: hash,
		Email:     email,
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
	}))
	return plain
}

func uniqueEmail(prefix string) string { return prefix + "-" + uuid.NewString() + "@example.test" }

const testPassword = "hunter2-hunter2"

func TestAcceptInviteThenLoginThenResolve(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	now := time.Now()
	email := uniqueEmail("user")

	sessionToken, accountID, err := svc.AcceptInvite(ctx, mintInvite(t, email), email, testPassword, now)
	require.NoError(t, err)
	require.NotEmpty(t, sessionToken)
	require.NotEqual(t, uuid.Nil, accountID)

	got, err := svc.ResolveSession(ctx, sessionToken, now)
	require.NoError(t, err)
	require.Equal(t, accountID, got)

	loginToken, loginAccount, err := svc.Login(ctx, email, testPassword, now)
	require.NoError(t, err)
	require.Equal(t, accountID, loginAccount)
	require.NotEqual(t, sessionToken, loginToken, "each login must mint a fresh session")
}

// An invite is single-use. Reuse would mean one leaked link creates unlimited accounts.
func TestInviteCannotBeConsumedTwice(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	now := time.Now()
	email := uniqueEmail("once")
	invite := mintInvite(t, email)

	_, _, err := svc.AcceptInvite(ctx, invite, email, testPassword, now)
	require.NoError(t, err)

	_, _, err = svc.AcceptInvite(ctx, invite, email, testPassword, now)
	require.ErrorIs(t, err, auth.ErrInvalidInvite)
}

// The invite is bound to the address it was issued for; presenting it with a different
// address must not create an account.
func TestInviteIsBoundToItsEmail(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	invite := mintInvite(t, uniqueEmail("bound"))

	_, _, err := svc.AcceptInvite(ctx, invite, uniqueEmail("someone-else"), testPassword, time.Now())
	require.ErrorIs(t, err, auth.ErrInvalidInvite)
}

func TestExpiredInviteIsRejected(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	email := uniqueEmail("stale")

	_, _, err := svc.AcceptInvite(ctx, mintInvite(t, email), email, testPassword,
		time.Now().Add(8*24*time.Hour))
	require.ErrorIs(t, err, auth.ErrInvalidInvite)
}

func TestLoginRejectsWrongPasswordAndUnknownEmail(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	now := time.Now()
	email := uniqueEmail("wrong")
	_, _, err := svc.AcceptInvite(ctx, mintInvite(t, email), email, testPassword, now)
	require.NoError(t, err)

	_, _, err = svc.Login(ctx, email, "not-the-password", now)
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)

	_, _, err = svc.Login(ctx, uniqueEmail("nobody"), "x", now)
	require.ErrorIs(t, err, auth.ErrInvalidCredentials,
		"an unknown email must be indistinguishable from a wrong password")
}

// K16: sessions are revocable. This is the property a JWT could not give us.
func TestLogoutRevokesImmediately(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	now := time.Now()
	email := uniqueEmail("revoke")
	token, _, err := svc.AcceptInvite(ctx, mintInvite(t, email), email, testPassword, now)
	require.NoError(t, err)

	require.NoError(t, svc.Logout(ctx, token))
	_, err = svc.ResolveSession(ctx, token, now)
	require.ErrorIs(t, err, auth.ErrNoSession)
}

func TestExpiredSessionIsRejected(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	now := time.Now()
	email := uniqueEmail("expire")
	token, _, err := svc.AcceptInvite(ctx, mintInvite(t, email), email, testPassword, now)
	require.NoError(t, err)

	_, err = svc.ResolveSession(ctx, token, now.Add(25*time.Hour))
	require.ErrorIs(t, err, auth.ErrNoSession)
}

// The account prefix in a session token is a routing hint, not a credential. Rewriting it
// must not move the session to another account, and must not resolve at all.
func TestForgedAccountPrefixDoesNotResolve(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	now := time.Now()

	emailA, emailB := uniqueEmail("victim"), uniqueEmail("attacker")
	tokenA, _, err := svc.AcceptInvite(ctx, mintInvite(t, emailA), emailA, testPassword, now)
	require.NoError(t, err)
	_, accountB, err := svc.AcceptInvite(ctx, mintInvite(t, emailB), emailB, testPassword, now)
	require.NoError(t, err)

	_, secret, ok := auth.SplitSessionToken(tokenA)
	require.True(t, ok)

	// Attacker points the victim's secret at their own account, then at a random one.
	for _, forgedAccount := range []uuid.UUID{accountB, uuid.New()} {
		forged := auth.JoinSessionToken(forgedAccount, secret)
		_, err := svc.ResolveSession(ctx, forged, now)
		require.ErrorIs(t, err, auth.ErrNoSession, "forged prefix %s resolved", forgedAccount)
	}

	// The untouched token still works, so the test failed for the right reason.
	got, err := svc.ResolveSession(ctx, tokenA, now)
	require.NoError(t, err)
	require.NotEqual(t, accountB, got)
}

func TestMalformedSessionTokenIsRejected(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	for _, bad := range []string{"", "garbage", "no-dot-here", "!!!.xyz"} {
		_, err := svc.ResolveSession(ctx, bad, time.Now())
		require.ErrorIs(t, err, auth.ErrNoSession, "input %q", bad)
	}
}

// A disabled account must not be able to log in, and the refusal must be the same one an
// unknown email gets.
func TestDisabledAccountCannotLogIn(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	now := time.Now()
	email := uniqueEmail("disabled")
	_, accountID, err := svc.AcceptInvite(ctx, mintInvite(t, email), email, testPassword, now)
	require.NoError(t, err)

	owner, err := store.NewPool(ctx, os.Getenv("PLIMSOLL_OWNER_DSN"))
	require.NoError(t, err)
	defer owner.Close()
	tx, err := owner.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('app.account_id', $1, true)`, accountID.String())
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE accounts SET disabled_at = now() WHERE id = $1`, accountID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	_, _, err = svc.Login(ctx, email, testPassword, now)
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
}

// The app role must not be able to read the credential index directly -- the SECURITY
// DEFINER function is the entire path.
func TestAppRoleCannotReadCredentialsDirectly(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)

	for _, table := range []string{"invites", "account_credentials"} {
		var has bool
		err := pool.QueryRow(ctx,
			`SELECT has_table_privilege(current_user, $1, 'SELECT')`, table).Scan(&has)
		require.NoError(t, err)
		require.False(t, has, "plimsoll_app must not be able to read %s", table)
	}
}
