package authservice

import (
	"context"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// placeholderAttempts is uniquePlaceholderUsername's maxAttempts.
const placeholderAttempts = 5

var errNoPlaceholder = wire.NewError(http.StatusInternalServerError, "could not allocate placeholder username")

// UpsertOAuthUser is DefaultAuthService.upsertOAuthUser, the shared path of
// every passwordless and OAuth sign-in (Apple, Google, X, phone, magic link):
//  1. an identity for (provider, providerUserID) whose user exists signs in;
//  2. else a user with this email gets the identity linked to it;
//  3. else a new user with a random password, a placeholder username, a trial
//     and a personal vault, then the identity and the Hermes profile.
//
// Swift weaknesses, kept: step 2 links by email whatever emailVerified says and
// whatever the provider, so an identity that asserts someone else's address
// (an X account with an unverified email, say) signs in to that account. The
// writes are not one transaction, and a concurrent first sign-in loses on the
// (provider, provider_user_id) unique index with a 500.
func (s *Service) UpsertOAuthUser(ctx context.Context, provider, providerUserID, email string, emailVerified bool) (*User, error) {
	// Hummingbird runs a handler to the end after the client leaves; finish the
	// sequence so no user is left without its identity, trial or vault.
	ctx = context.WithoutCancel(ctx)
	var tenant uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT tenant_id FROM oauth_identities WHERE provider = $1 AND provider_user_id = $2 LIMIT 1`,
		provider, providerUserID).Scan(&tenant)
	switch {
	case err == nil:
		u, err := s.FindUserByID(ctx, tenant)
		if err != nil {
			return nil, err
		}
		if u != nil {
			return u, nil
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}

	u, err := s.FindUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if u != nil {
		if err := s.insertIdentity(ctx, u, provider, providerUserID, email, emailVerified); err != nil {
			return nil, err
		}
		return u, nil
	}

	hash, err := auth.HashPassword(uuid.NewString()+uuid.NewString(), s.opt.PasswordCost)
	if err != nil {
		return nil, err
	}
	username, err := s.uniquePlaceholderUsername(ctx)
	if err != nil {
		return nil, err
	}
	u, err = s.CreateUser(ctx, email, username, hash)
	if err != nil {
		return nil, err
	}
	if err := s.insertIdentity(ctx, u, provider, providerUserID, email, emailVerified); err != nil {
		return nil, err
	}
	s.EnsureSoft(ctx, u)
	return u, nil
}

// insertIdentity saves an OAuthIdentity: its init lowercases the email.
func (s *Service) insertIdentity(ctx context.Context, u *User, provider, providerUserID, email string, emailVerified bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO oauth_identities (id, tenant_id, provider, provider_user_id, email, email_verified, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New(), u.ID, provider, providerUserID, swiftstr.Lowercased(email), emailVerified, s.opt.Now())
	return err
}

// uniquePlaceholderUsername draws oauth-<8hex> until one is free, five tries.
func (s *Service) uniquePlaceholderUsername(ctx context.Context) (string, error) {
	for range placeholderAttempts {
		candidate := PlaceholderUsername()
		existing, err := s.FindUserByUsername(ctx, candidate)
		if err != nil {
			return "", err
		}
		if existing == nil {
			return candidate, nil
		}
	}
	return "", errNoPlaceholder
}
