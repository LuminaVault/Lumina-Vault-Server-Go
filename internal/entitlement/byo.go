package entitlement

import (
	"context"
	"errors"
	"lumina/internal/secretbox"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
// One known difference, only reachable with billing enforcement on: Swift's
// store hands out one candidate per call, rotating across primary and pool
// keys with a per-process cursor that chat traffic also advances. With a mix
// of spendable and unspendable candidates its answer alternates; this answers
// whether any candidate is spendable, which is one of Swift's two answers.
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

// spendable is loadCandidates followed by isSpendable. Swift builds the whole
// candidate list before answering and its caller wraps that in try?, so any
// read or decrypt failure, a pool key's included, drops the whole provider
// even when another candidate is good.
func spendable(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, tenant uuid.UUID, provider string) bool {
	var kind string
	var ct, nonce []byte
	var baseURL *string
	hasPrimary := true
	err := pool.QueryRow(ctx, `SELECT credential_kind, ciphertext, nonce, base_url FROM user_provider_credentials
		WHERE tenant_id = $1 AND provider = $2 LIMIT 1`, tenant, provider).Scan(&kind, &ct, &nonce, &baseURL)
	if errors.Is(err, pgx.ErrNoRows) {
		hasPrimary = false
	} else if err != nil {
		return false
	}
	if hasPrimary && provider == "xai" && kind == "oauth" {
		if key, ok := linkedXAIKey(ctx, pool, box, tenant); ok {
			return isSpendable(provider, &key, nil) // the only candidate
		}
	}
	var primaryURL *url.URL
	if baseURL != nil {
		primaryURL = parseURL(*baseURL)
	}
	anySpendable := false
	if hasPrimary {
		var key *string
		if ct != nil && nonce != nil {
			k, err := box.Open(secretbox.Sealed{Ciphertext: ct, Nonce: nonce}, tenant)
			if err != nil {
				return false
			}
			key = &k
		}
		anySpendable = isSpendable(provider, key, primaryURL)
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
		anySpendable = anySpendable || isSpendable(provider, &k, primaryURL)
	}
	return rows.Err() == nil && anySpendable
}

// linkedXAIKey is the SuperGrok marker path: an "oauth" xai row resolves
// through the tenant's Hermes container once it has completed the xAI connect.
// Any failure means no handle (Swift's try? on containerManager.handle), and
// the marker row then falls through to the normal candidates.
func linkedXAIKey(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, tenant uuid.UUID) (string, bool) {
	var ct, nonce []byte
	var connectedAt *time.Time
	err := pool.QueryRow(ctx, `SELECT api_server_key_ciphertext, api_server_key_nonce, xai_connected_at
		FROM hermes_tenant_containers WHERE tenant_id = $1 LIMIT 1`, tenant).Scan(&ct, &nonce, &connectedAt)
	if err != nil {
		return "", false
	}
	key, err := box.Open(secretbox.Sealed{Ciphertext: ct, Nonce: nonce}, tenant)
	if err != nil || connectedAt == nil {
		return "", false
	}
	return key, true
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
