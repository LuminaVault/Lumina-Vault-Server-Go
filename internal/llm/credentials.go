package llm

import (
	"context"
	"errors"
	"lumina/internal/secretbox"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CredentialStore is the read half of UserCredentialStore: it reads
// user_provider_credentials and its pool keys, decrypts them, caches the
// candidate list per tenant and provider, and rotates across it on each call.
// The write half moves with me-providers in wave 5.
type CredentialStore struct {
	pool        *pgxpool.Pool
	box         *secretbox.Box
	xaiResolver XAIContainerResolver
	ttl         time.Duration
	now         func() time.Time

	mu       sync.Mutex
	cache    map[credentialKey]credentialEntry
	rotation map[credentialKey]int
}

type credentialKey struct {
	tenant   uuid.UUID
	provider ProviderKind
}

type credentialEntry struct {
	candidates []ResolvedCredential
	expires    time.Time
}

// NewCredentialStore needs a non-nil box. xaiResolver may be nil.
func NewCredentialStore(pool *pgxpool.Pool, box *secretbox.Box, xaiResolver XAIContainerResolver) *CredentialStore {
	return &CredentialStore{
		pool: pool, box: box, xaiResolver: xaiResolver, ttl: cacheTTL, now: time.Now,
		cache: map[credentialKey]credentialEntry{}, rotation: map[credentialKey]int{},
	}
}

// Invalidate drops a single cache entry.
func (s *CredentialStore) Invalidate(tenant uuid.UUID, provider ProviderKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, credentialKey{tenant, provider})
}

// Credential returns nil with no error when there is no usable row. With a
// round-robin pool it rotates across primary and pool keys per call: the list
// is cached while the cursor advances live, and the cursor resets on restart.
func (s *CredentialStore) Credential(ctx context.Context, provider ProviderKind, tenant uuid.UUID) (*ResolvedCredential, error) {
	key := credentialKey{tenant, provider}
	s.mu.Lock()
	entry, ok := s.cache[key]
	s.mu.Unlock()
	if !ok || !entry.expires.After(s.now()) {
		candidates, err := s.loadCandidates(ctx, provider, tenant)
		if err != nil {
			return nil, err
		}
		entry = credentialEntry{candidates, s.now().Add(s.ttl)}
		s.mu.Lock()
		s.cache[key] = entry
		s.mu.Unlock()
	}
	switch len(entry.candidates) {
	case 0:
		return nil, nil
	case 1:
		return &entry.candidates[0], nil
	}
	s.mu.Lock()
	idx := s.rotation[key] % len(entry.candidates)
	s.rotation[key] = idx + 1
	s.mu.Unlock()
	return &entry.candidates[idx], nil
}

func (s *CredentialStore) loadCandidates(ctx context.Context, provider ProviderKind, tenant uuid.UUID) ([]ResolvedCredential, error) {
	var kind string
	var ct, nonce []byte
	var baseURL, label *string
	hasPrimary := true
	err := s.pool.QueryRow(ctx, `SELECT credential_kind, ciphertext, nonce, base_url, label FROM user_provider_credentials
		WHERE tenant_id = $1 AND provider = $2 LIMIT 1`, tenant, provider).Scan(&kind, &ct, &nonce, &baseURL, &label)
	if errors.Is(err, pgx.ErrNoRows) {
		hasPrimary = false
	} else if err != nil {
		return nil, err
	}

	// SuperGrok marker row: talk to the tenant's gateway, which holds the linked
	// xAI session. The row itself carries no secret.
	if hasPrimary && provider == XAI && kind == "oauth" && s.xaiResolver != nil {
		if handle := s.xaiResolver(ctx, tenant); handle != nil && handle.XAIConnectedAt != nil {
			return []ResolvedCredential{{APIKey: &handle.APIServerKey, BaseURL: parseOptionalURL(&handle.BaseURL), Label: label}}, nil
		}
	}

	var candidates []ResolvedCredential
	primaryURL := parseOptionalURL(baseURL)
	if hasPrimary {
		var apiKey *string
		if ct != nil && nonce != nil {
			plain, err := s.box.Open(secretbox.Sealed{Ciphertext: ct, Nonce: nonce}, tenant)
			if err != nil {
				return nil, err
			}
			apiKey = &plain
		}
		if apiKey != nil || primaryURL != nil {
			candidates = append(candidates, ResolvedCredential{APIKey: apiKey, BaseURL: primaryURL, Label: label})
		}
	}

	rows, err := s.pool.Query(ctx, `SELECT ciphertext, nonce, label FROM user_provider_credential_pool_keys
		WHERE tenant_id = $1 AND provider = $2 ORDER BY created_at`, tenant, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pct, pnonce []byte
		var plabel *string
		if err := rows.Scan(&pct, &pnonce, &plabel); err != nil {
			return nil, err
		}
		if pct == nil || pnonce == nil {
			continue
		}
		plain, err := s.box.Open(secretbox.Sealed{Ciphertext: pct, Nonce: pnonce}, tenant)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, ResolvedCredential{APIKey: &plain, BaseURL: primaryURL, Label: plabel})
	}
	return candidates, rows.Err()
}
