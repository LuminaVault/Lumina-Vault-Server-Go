package entitlement

import (
	"context"
	"lumina/internal/secretbox"
	"net/url"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// userCredentialTargets is ProviderKind.userCredentialTargets.
var userCredentialTargets = []string{"xai", "anthropic", "openai", "gemini", "openRouter", "ollama", "nvidia", "nous", "custom"}

// keylessCapable is ProviderKind.keylessCapable: a base URL alone is a
// credential for a server the user runs.
var keylessCapable = map[string]bool{"ollama": true, "custom": true}

// PGCredentials is App+build.swift's byoHasUsableCredential over
// UserCredentialStore: does any user-credential provider hold a spendable
// candidate (the primary row, then pool keys inheriting its base URL)?
// A nil box (no LV_SECRET_MASTER_KEY) answers false, as Swift's nil store does.
//
// Known differences from Swift, both only reachable with billing enforcement
// on: Swift's store returns one candidate per call, rotating across pool
// keys (so with a mix of spendable and unspendable candidates its answer
// alternates); this checks whether any candidate is spendable. And the xAI
// SuperGrok "oauth" marker row, which Swift resolves through the tenant's
// Hermes container, never counts here (the container lookup is wave 6).
func PGCredentials(pool *pgxpool.Pool, box *secretbox.Box) HasUsableCredential {
	return func(ctx context.Context, tenant uuid.UUID) bool {
		if box == nil {
			return false
		}
		for _, provider := range userCredentialTargets {
			if spendable(ctx, pool, box, tenant, provider) {
				return true
			}
		}
		return false
	}
}

func spendable(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, tenant uuid.UUID, provider string) bool {
	var ct, nonce []byte
	var baseURL *string
	hasPrimary := true
	err := pool.QueryRow(ctx, `SELECT ciphertext, nonce, base_url FROM user_provider_credentials
		WHERE tenant_id = $1 AND provider = $2 LIMIT 1`, tenant, provider).Scan(&ct, &nonce, &baseURL)
	if err != nil {
		hasPrimary = false // no row; a read failure is a missing credential, as Swift's try? makes it
	}
	var primaryURL *url.URL
	if baseURL != nil {
		primaryURL = parseURL(*baseURL)
	}
	if hasPrimary {
		var key *string
		if ct != nil && nonce != nil {
			k, err := box.Open(secretbox.Sealed{Ciphertext: ct, Nonce: nonce}, tenant)
			if err != nil {
				return false // Swift: decode throws, try? drops the whole provider
			}
			key = &k
		}
		if (key != nil || primaryURL != nil) && isSpendable(provider, key, primaryURL) {
			return true
		}
	}
	rows, err := pool.Query(ctx, `SELECT ciphertext, nonce FROM user_provider_credential_pool_keys
		WHERE tenant_id = $1 AND provider = $2 ORDER BY created_at`, tenant, provider)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var pct, pnonce []byte
		if rows.Scan(&pct, &pnonce) != nil {
			return false
		}
		if pct == nil || pnonce == nil {
			continue
		}
		k, err := box.Open(secretbox.Sealed{Ciphertext: pct, Nonce: pnonce}, tenant)
		if err != nil {
			return false
		}
		if isSpendable(provider, &k, primaryURL) {
			return true
		}
	}
	return false
}

// isSpendable is ProviderKind.isSpendable(apiKey:baseURL:).
func isSpendable(provider string, apiKey *string, baseURL *url.URL) bool {
	if apiKey != nil && *apiKey != "" {
		return true
	}
	return keylessCapable[provider] && baseURL != nil && baseURL.Scheme != "" && baseURL.Host != ""
}

// parseURL approximates Foundation's URL(string:): nil when it does not parse.
func parseURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		return nil
	}
	return u
}
