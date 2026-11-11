package meproviders

import (
	"encoding/json"
	"lumina/internal/llm"
	"lumina/internal/wire"
	"reflect"
	"slices"
	"time"
)

// providerIDs is ProviderID.allCases, in declaration order: the list and the
// connections summary are built in it.
var providerIDs = []llm.ProviderID{"xai", "anthropic", "openai", "ollama", "openRouter", "nvidia", "gemini", "nous", "custom"}

// unmarshalEnum is a Swift String enum's init(from:): a non-string is a type
// mismatch against String, and an unknown string is dataCorrupted naming the type.
func unmarshalEnum(b []byte, swiftType string, allowed []string) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if !slices.Contains(allowed, s) {
		return "", &wire.CorruptedError{Message: "Cannot initialize " + swiftType + " from invalid String value " + s}
	}
	return s, nil
}

// providerID is ProviderID in a request body.
type providerID string

func (p *providerID) UnmarshalJSON(b []byte) error {
	allowed := make([]string, len(providerIDs))
	for i, id := range providerIDs {
		allowed[i] = string(id)
	}
	s, err := unmarshalEnum(b, "ProviderID", allowed)
	*p = providerID(s)
	return err
}

// credentialKind is ProviderCredentialKind.
type credentialKind string

const (
	kindAPIKey  credentialKind = "apiKey"
	kindOAuth   credentialKind = "oauth"
	kindHostURL credentialKind = "hostURL"
)

func (k *credentialKind) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "ProviderCredentialKind", []string{"apiKey", "oauth", "hostURL"})
	*k = credentialKind(s)
	return err
}

func validCredentialKind(s string) bool {
	return s == string(kindAPIKey) || s == string(kindOAuth) || s == string(kindHostURL)
}

// brainMode is LLMBrainMode.
type brainMode string

func (m *brainMode) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "LLMBrainMode", []string{"managed", "byok"})
	*m = brainMode(s)
	return err
}

// routingPolicy is LLMRoutingPolicy. The preferences PUT decodes it and ignores it.
type routingPolicy string

func (p *routingPolicy) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "LLMRoutingPolicy", []string{"locked", "autoSmart", "fastCheap", "balanced", "maxQuality"})
	*p = routingPolicy(s)
	return err
}

// MARK: - Requests

// credentialPutRequest is ProviderCredentialPutRequest.
type credentialPutRequest struct {
	Kind    credentialKind `json:"kind"`
	APIKey  *string        `json:"apiKey,omitempty"`
	BaseURL *string        `json:"baseUrl,omitempty"`
	Label   *string        `json:"label,omitempty"`
}

// poolAddRequest is ProviderPoolAddRequest.
type poolAddRequest struct {
	APIKey string  `json:"apiKey"`
	Label  *string `json:"label,omitempty"`
}

// modelRoute is ModelRouteDTO.
type modelRoute struct {
	Provider providerID `json:"provider"`
	Model    string     `json:"model"`
}

// preferencesPutRequest is LLMPreferencesPutRequest, whose init(from:) defaults
// mode to managed and the provider lists to empty.
type preferencesPutRequest struct {
	Mode             *brainMode     `json:"mode,omitempty"`
	PrimaryProvider  providerID     `json:"primaryProvider"`
	PrimaryModel     string         `json:"primaryModel"`
	FallbackChain    []modelRoute   `json:"fallbackChain"`
	AllowedProviders []providerID   `json:"allowedProviders,omitempty"`
	BlockedProviders []providerID   `json:"blockedProviders,omitempty"`
	RoutingPolicy    *routingPolicy `json:"routingPolicy,omitempty"`
}

// MARK: - Responses

// credentialDTO is ProviderCredentialDTO. Nil optionals stay out, as Codable leaves them.
type credentialDTO struct {
	Provider        llm.ProviderID `json:"provider"`
	Kind            string         `json:"kind"`
	HasCredential   bool           `json:"hasCredential"`
	BaseURL         *string        `json:"baseUrl,omitempty"`
	Label           *string        `json:"label,omitempty"`
	VerifiedAt      *wire.Time     `json:"verifiedAt,omitempty"`
	LastFailureAt   *wire.Time     `json:"lastFailureAt,omitempty"`
	LastFailureCode *string        `json:"lastFailureCode,omitempty"`
	SupportsTools   *bool          `json:"supportsTools,omitempty"`
	ToolsProbedAt   *wire.Time     `json:"toolsProbedAt,omitempty"`
}

type credentialsListResponse struct {
	Providers []credentialDTO `json:"providers"`
}

// catalogEntryDTO is ProviderCatalogEntryDTO.
type catalogEntryDTO struct {
	Provider        llm.ProviderID `json:"provider"`
	DisplayName     string         `json:"displayName"`
	DefaultBaseURL  *string        `json:"defaultBaseURL,omitempty"`
	RequiresBaseURL bool           `json:"requiresBaseURL"`
	RequiresAPIKey  bool           `json:"requiresAPIKey"`
	KeyHint         *string        `json:"keyHint,omitempty"`
	KeysURL         *string        `json:"keysURL,omitempty"`
	Available       bool           `json:"available"`
}

type catalogResponse struct {
	Providers []catalogEntryDTO `json:"providers"`
}

// testResponse is ProviderTestResponse.
type testResponse struct {
	VerifiedAt wire.Time `json:"verifiedAt"`
	Model      *string   `json:"model,omitempty"`
}

// modelInfoDTO is LLMModelInfo.
type modelInfoDTO struct {
	ID            string `json:"id"`
	DisplayName   string `json:"displayName"`
	ContextWindow *int   `json:"contextWindow,omitempty"`
	Tier          string `json:"tier"`
}

// modelsResponse is ProviderModelsResponse.
type modelsResponse struct {
	Provider    llm.ProviderID `json:"provider"`
	Models      []modelInfoDTO `json:"models"`
	FetchedLive bool           `json:"fetchedLive"`
}

// poolKeyDTO is ProviderPoolKeyDTO.
type poolKeyDTO struct {
	ID        wire.UUID  `json:"id"`
	Label     *string    `json:"label,omitempty"`
	CreatedAt *wire.Time `json:"createdAt,omitempty"`
}

type poolListResponse struct {
	Provider llm.ProviderID `json:"provider"`
	Keys     []poolKeyDTO   `json:"keys"`
}

// routeDTO is ModelRouteDTO on the way out.
type routeDTO struct {
	Provider llm.ProviderID `json:"provider"`
	Model    string         `json:"model"`
}

// preferencesResponse is LLMPreferencesGetResponse. Every field is encoded:
// the synthesized encoder writes the defaulted ones too.
type preferencesResponse struct {
	Mode             string           `json:"mode"`
	PrimaryProvider  llm.ProviderID   `json:"primaryProvider"`
	PrimaryModel     string           `json:"primaryModel"`
	FallbackChain    []routeDTO       `json:"fallbackChain"`
	AllowedProviders []llm.ProviderID `json:"allowedProviders"`
	BlockedProviders []llm.ProviderID `json:"blockedProviders"`
	RoutingPolicy    string           `json:"routingPolicy"`
}

// wireTime is a nullable timestamp column as an optional Date.
func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}
