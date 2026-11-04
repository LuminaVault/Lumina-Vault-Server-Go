package embedding

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"lumina/internal/embedding/db"
	"lumina/internal/secretbox"
	"lumina/internal/swiftstr"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	byokEmbeddingsSlug = "byok-embeddings"
	byokConfigKey      = "access_token"
	byokCacheTTL       = 300 * time.Second
)

// perTenantResolving is PerTenantEmbeddingResolving. A nil Service means the
// tenant has none.
type perTenantResolving interface {
	service(ctx context.Context, tenant uuid.UUID) Service
}

// tenantAware is TenantAwareEmbeddingService. A tenant's own service is used bare:
// their embeds skip the platform's cap, usage rows and fallback chain, as in Swift.
type tenantAware struct {
	global   Service
	resolver perTenantResolving
}

func (t *tenantAware) Embed(ctx context.Context, text string, tenant uuid.UUID) ([]float32, error) {
	if own := t.resolver.service(ctx, tenant); own != nil {
		return own.Embed(ctx, text, tenant)
	}
	return t.global.Embed(ctx, text, tenant)
}

type cachedTenantService struct {
	service Service
	at      time.Time
}

// perTenantResolver is PerTenantEmbeddingResolver: the enabled byok-embeddings
// install, its key opened with SecretBox. Results, including "none", are cached
// for the TTL so the hot path doesn't query on every embed, and key or install
// changes take up to the TTL to show. A failed read is also "none" and also cached.
type perTenantResolver struct {
	q     *db.Queries
	box   *secretbox.Box
	keyed func(apiKey string) Service
	ttl   time.Duration
	now   func() time.Time

	mu    sync.Mutex
	cache map[uuid.UUID]cachedTenantService
}

func newPerTenantResolver(q *db.Queries, box *secretbox.Box, keyed func(string) Service) *perTenantResolver {
	return &perTenantResolver{q: q, box: box, keyed: keyed, ttl: byokCacheTTL, now: time.Now, cache: map[uuid.UUID]cachedTenantService{}}
}

func (r *perTenantResolver) service(ctx context.Context, tenant uuid.UUID) Service {
	r.mu.Lock()
	cached, ok := r.cache[tenant]
	r.mu.Unlock()
	if ok && r.now().Sub(cached.at) < r.ttl {
		return cached.service
	}
	built := r.build(ctx, tenant)
	r.mu.Lock()
	r.cache[tenant] = cachedTenantService{built, r.now()}
	r.mu.Unlock()
	return built
}

func (r *perTenantResolver) build(ctx context.Context, tenant uuid.UUID) Service {
	row, err := r.q.EnabledPluginConfig(ctx, db.EnabledPluginConfigParams{TenantID: tenant, PluginSlug: byokEmbeddingsSlug})
	if err != nil {
		return nil
	}
	key, ok := r.openKey(row, tenant)
	if !ok {
		slog.WarnContext(ctx, fmt.Sprintf("byok-embeddings install present but config unreadable tenant=%s", tenant))
		return nil
	}
	return r.keyed(key)
}

// openKey reads the key the way Swift decodes [String: String]: one value that
// isn't a string makes the whole config unreadable.
func (r *perTenantResolver) openKey(row db.EnabledPluginConfigRow, tenant uuid.UUID) (string, bool) {
	plaintext, err := r.box.Open(secretbox.Sealed{Ciphertext: row.ConfigCiphertext, Nonce: row.ConfigNonce}, tenant)
	if err != nil {
		return "", false
	}
	var config map[string]*string
	if err := json.Unmarshal([]byte(plaintext), &config); err != nil {
		return "", false
	}
	for _, value := range config {
		if value == nil {
			return "", false
		}
	}
	value := config[byokConfigKey]
	if value == nil {
		return "", false
	}
	key := swiftstr.TrimWhitespacesAndNewlines(*value)
	return key, key != ""
}
