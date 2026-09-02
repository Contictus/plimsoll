package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/store"
	"github.com/Contictus/plimsoll/backend/internal/tenancy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrInvalidCredentials covers a wrong password, an unknown email, and a disabled
	// account alike. Distinguishing them would turn login into a user-enumeration oracle.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")

	// ErrInvalidInvite covers unknown, expired, already-consumed, and wrong-address.
	ErrInvalidInvite = errors.New("auth: invite is unknown, expired, or already used")

	// ErrNoSession covers a malformed token, an unknown one, and an expired one.
	ErrNoSession = errors.New("auth: no valid session")
)

// Service performs the operations that run before a tenant context exists, and therefore
// cannot go through tenancy.InTx. Each one reaches the database through a SECURITY
// DEFINER function (migration 00003) rather than through a table grant.
//
// Time is a parameter on every method (L4): session expiry is testable without sleeping.
type Service struct {
	q          *store.Queries
	db         tenancy.Beginner
	sessionTTL time.Duration
}

// NewService wires the pre-auth queries and the tenant-scoped connection used for the one
// post-authentication check login makes. It deliberately takes no pool: holding one would
// let this package reach tenant data without tenancy.InTx (K15).
func NewService(q *store.Queries, db tenancy.Beginner, sessionTTL time.Duration) *Service {
	return &Service{q: q, db: db, sessionTTL: sessionTTL}
}

// Login verifies an email and password and issues a fresh session.
func (s *Service) Login(
	ctx context.Context, email, password string, now time.Time,
) (string, uuid.UUID, error) {
	cred, err := s.q.LookupCredentials(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		// Burn a hash anyway, so a missing account and a wrong password take the same
		// time and the endpoint cannot be used to enumerate users.
		DummyVerify()
		return "", uuid.Nil, ErrInvalidCredentials
	}
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("auth: lookup credentials: %w", err)
	}

	ok, err := VerifyPassword(cred.PasswordHash, password)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("auth: verify (account %s): %w", cred.AccountID, err)
	}
	if !ok {
		return "", uuid.Nil, ErrInvalidCredentials
	}

	// The disabled check lives here rather than inside the SQL function: it is a policy
	// decision, and by this point the account id is known, so it can be read the normal
	// way -- under RLS, through tenancy.InTx.
	var disabled bool
	err = tenancy.InTx(ctx, s.db, cred.AccountID, func(q *store.Queries) error {
		account, err := q.GetAccountByID(ctx, cred.AccountID)
		if err != nil {
			return err
		}
		disabled = account.DisabledAt != nil
		return nil
	})
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("auth: load account %s: %w", cred.AccountID, err)
	}
	if disabled {
		return "", uuid.Nil, ErrInvalidCredentials
	}

	token, err := s.issueSession(ctx, cred.AccountID, now)
	if err != nil {
		return "", uuid.Nil, err
	}
	return token, cred.AccountID, nil
}

// AcceptInvite consumes an invite and creates the account it was issued for, then logs the
// new account in. The account, its email and its credential row are written by one SQL
// function so that a crash cannot leave a consumed invite with no account behind it.
func (s *Service) AcceptInvite(
	ctx context.Context, inviteToken, email, password string, now time.Time,
) (string, uuid.UUID, error) {
	encoded, err := HashPassword(password)
	if err != nil {
		return "", uuid.Nil, err
	}

	accountID, err := s.q.ConsumeInvite(ctx, store.ConsumeInviteParams{
		TokenHash:    HashToken(inviteToken),
		Email:        email,
		PasswordHash: encoded,
		Now:          now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, ErrInvalidInvite
	}
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("auth: consume invite: %w", err)
	}

	token, err := s.issueSession(ctx, accountID, now)
	if err != nil {
		return "", uuid.Nil, err
	}
	return token, accountID, nil
}

// ResolveSession turns a session token into the account it belongs to, refreshing
// last_seen_at as a side effect.
//
// The account id parsed from the token is an unverified claim; it is passed to the SQL
// function only so the sessions read is possible at all under FORCE ROW LEVEL SECURITY.
// The row lookup on the token hash is what verifies it -- a forged prefix binds an
// account whose policy hides every row, so it resolves to nothing.
func (s *Service) ResolveSession(
	ctx context.Context, token string, now time.Time,
) (uuid.UUID, error) {
	claimed, err := AccountFromSessionToken(token)
	if err != nil {
		return uuid.Nil, ErrNoSession
	}

	accountID, err := s.q.ResolveSession(ctx, store.ResolveSessionParams{
		AccountID: claimed,
		TokenHash: HashToken(token),
		Now:       now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNoSession
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: resolve session: %w", err)
	}
	if accountID == uuid.Nil {
		return uuid.Nil, ErrNoSession
	}
	return accountID, nil
}

// Logout revokes a session immediately. A token that no longer has a row is dead the
// instant this returns, which is the property K16 chose opaque sessions over JWTs to get.
func (s *Service) Logout(ctx context.Context, token string) error {
	claimed, err := AccountFromSessionToken(token)
	if err != nil {
		// Nothing to revoke, and nothing the caller can do about it.
		return nil
	}
	if err := s.q.DeleteSession(ctx, store.DeleteSessionParams{
		AccountID: claimed,
		TokenHash: HashToken(token),
	}); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// SessionTTL is how long an issued session stays valid. The HTTP layer needs it to set the
// cookie expiry to the same instant the row expires.
func (s *Service) SessionTTL() time.Duration { return s.sessionTTL }

// issueSession mints a fresh token. The plaintext is returned to the caller and never
// stored; only its hash reaches Postgres (K16).
func (s *Service) issueSession(
	ctx context.Context, accountID uuid.UUID, now time.Time,
) (string, error) {
	plain, hash, err := NewSessionToken(accountID)
	if err != nil {
		return "", err
	}
	err = s.q.CreateSession(ctx, store.CreateSessionParams{
		AccountID: accountID,
		TokenHash: hash,
		ExpiresAt: now.Add(s.sessionTTL),
	})
	if err != nil {
		return "", fmt.Errorf("auth: create session for %s: %w", accountID, err)
	}
	return plain, nil
}
