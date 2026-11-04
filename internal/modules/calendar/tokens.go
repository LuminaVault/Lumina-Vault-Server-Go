package calendar

import (
	"context"
	"errors"
	"lumina/internal/modules/calendar/db"
	"lumina/internal/secretbox"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const refreshSkew = 120 * time.Second

// Errors of CalendarTokenStore.Error.
var (
	errNotConnected        = errors.New("calendar: not connected")
	errNeedsReauth         = errors.New("calendar: needs reauth")
	errMissingRefreshToken = errors.New("calendar: missing refresh token")
)

type refreshCall struct {
	done  chan struct{}
	token string
	err   error
}

// tokenStore is CalendarTokenStore. Tokens use the same SecretBox envelope and
// columns as Swift, so CalendarSyncWorker (which stays in Swift) reads what Go
// writes, and the other way round. Refresh is single-flight per tenant, per
// process: Go and the Swift worker can still refresh the same grant at once.
// That is harmless, since an earlier access token stays valid, and the last
// write to the row wins.
type tokenStore struct {
	q     *db.Queries
	box   *secretbox.Box
	oauth oauthClient
	now   func() time.Time

	mu       sync.Mutex
	inFlight map[uuid.UUID]*refreshCall
}

// storeInitialTokens upserts the (tenant, provider) row after the callback.
// Without a refresh token Swift refuses it: offline access would end on first
// expiry, and prompt=consent should always return one.
func (s *tokenStore) storeInitialTokens(ctx context.Context, tenant uuid.UUID, t tokenResponse, email *string) error {
	if t.refreshToken == nil {
		return errMissingRefreshToken
	}
	refresh, err := s.box.Seal(*t.refreshToken, tenant)
	if err != nil {
		return err
	}
	access, err := s.box.Seal(t.accessToken, tenant)
	if err != nil {
		return err
	}
	now := s.now()
	return s.q.StoreInitialTokens(ctx, db.StoreInitialTokensParams{
		ID:                uuid.New(),
		TenantID:          tenant,
		AccountEmail:      email,
		Scope:             *t.scope,
		RefreshCiphertext: refresh.Ciphertext,
		RefreshNonce:      refresh.Nonce,
		AccessCiphertext:  access.Ciphertext,
		AccessNonce:       access.Nonce,
		AccessExpiresAt:   now.Add(time.Duration(t.expiresIn) * time.Second),
		Now:               now,
	})
}

// validAccessToken returns a usable access token, refreshing when the cached
// one is within refreshSkew of expiry.
func (s *tokenStore) validAccessToken(ctx context.Context, tenant uuid.UUID) (string, error) {
	s.mu.Lock()
	call, ok := s.inFlight[tenant]
	s.mu.Unlock()
	if ok {
		<-call.done
		return call.token, call.err
	}
	a, err := s.q.GoogleAccount(ctx, tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotConnected
	}
	if err != nil {
		return "", err
	}
	if a.Status != "connected" {
		return "", errNeedsReauth
	}
	if a.AccessCiphertext != nil && a.AccessNonce != nil && a.AccessExpiresAt != nil && a.AccessExpiresAt.Sub(s.now()) > refreshSkew {
		return s.box.Open(secretbox.Sealed{Ciphertext: a.AccessCiphertext, Nonce: a.AccessNonce}, tenant)
	}
	s.mu.Lock()
	if call, ok := s.inFlight[tenant]; ok { // lost the race to start one
		s.mu.Unlock()
		<-call.done
		return call.token, call.err
	}
	call = &refreshCall{done: make(chan struct{})}
	s.inFlight[tenant] = call
	s.mu.Unlock()

	// A caller going away must not abandon a refresh others are waiting on.
	call.token, call.err = s.refresh(context.WithoutCancel(ctx), tenant, a)

	s.mu.Lock()
	delete(s.inFlight, tenant)
	s.mu.Unlock()
	close(call.done)
	return call.token, call.err
}

func (s *tokenStore) refresh(ctx context.Context, tenant uuid.UUID, a db.CalendarAccount) (string, error) {
	if a.RefreshCiphertext == nil || a.RefreshNonce == nil {
		return "", errMissingRefreshToken
	}
	refreshToken, err := s.box.Open(secretbox.Sealed{Ciphertext: a.RefreshCiphertext, Nonce: a.RefreshNonce}, tenant)
	if err != nil {
		return "", err
	}
	fresh, err := s.oauth.refresh(ctx, refreshToken)
	if errors.Is(err, errRefreshRejected) {
		// Swift ignores a failed write here too (try?).
		_ = s.q.MarkNeedsReauth(ctx, db.MarkNeedsReauthParams{TenantID: tenant, Now: s.now()})
		return "", errNeedsReauth
	}
	if err != nil {
		return "", err
	}
	access, err := s.box.Seal(fresh.accessToken, tenant)
	if err != nil {
		return "", err
	}
	now := s.now()
	params := db.StoreRefreshedAccessParams{
		TenantID:         tenant,
		AccessCiphertext: access.Ciphertext,
		AccessNonce:      access.Nonce,
		AccessExpiresAt:  now.Add(time.Duration(fresh.expiresIn) * time.Second),
		Now:              now,
	}
	// Google may rotate the refresh token; persist it if so.
	if fresh.refreshToken != nil {
		rotated, err := s.box.Seal(*fresh.refreshToken, tenant)
		if err != nil {
			return "", err
		}
		params.RefreshCiphertext = rotated.Ciphertext
		params.RefreshNonce = rotated.Nonce
	}
	if err := s.q.StoreRefreshedAccess(ctx, params); err != nil {
		return "", err
	}
	return fresh.accessToken, nil
}
