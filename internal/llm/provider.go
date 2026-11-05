package llm

// ProviderKind is Swift's ProviderKind: the upstream a route dispatches to. The
// values are stable: they appear in logs, metrics and provider_failover_events.
type ProviderKind string

const (
	HermesGateway  ProviderKind = "hermesGateway"
	Anthropic      ProviderKind = "anthropic"
	OpenAI         ProviderKind = "openai"
	Gemini         ProviderKind = "gemini"
	Together       ProviderKind = "together"
	Groq           ProviderKind = "groq"
	Fireworks      ProviderKind = "fireworks"
	DeepInfra      ProviderKind = "deepInfra"
	DeepseekDirect ProviderKind = "deepseekDirect"
	OpenRouter     ProviderKind = "openRouter"
	Deepseek       ProviderKind = "deepseek"
	Kimi           ProviderKind = "kimi"
	Ollama         ProviderKind = "ollama"
	XAI            ProviderKind = "xai"
	Nvidia         ProviderKind = "nvidia"
	Nous           ProviderKind = "nous"
	Custom         ProviderKind = "custom"
)

// ProviderID is LuminaVaultShared's ProviderID, the wire enum clients know.
type ProviderID string

// userCredentialTargets is ProviderKind.userCredentialTargets, which is also
// exactly the set with a ProviderID. Both share their raw values.
var userCredentialTargets = map[ProviderKind]bool{
	XAI: true, Anthropic: true, OpenAI: true, Gemini: true, OpenRouter: true,
	Ollama: true, Nvidia: true, Nous: true, Custom: true,
}

// UserCredentialTarget reports whether a tenant can attach a key to k.
func UserCredentialTarget(k ProviderKind) bool { return userCredentialTargets[k] }

// ToShared is ProviderKind.toShared: false for the internal routing targets
// (the gateway, Together, Groq and so on), which have no wire id.
func (k ProviderKind) ToShared() (ProviderID, bool) {
	if userCredentialTargets[k] {
		return ProviderID(k), true
	}
	return "", false
}

// FromShared is ProviderKind(shared:).
func FromShared(id ProviderID) (ProviderKind, bool) {
	k := ProviderKind(id)
	return k, userCredentialTargets[k]
}

// UserFacingName is the display name folded into fallback banners.
func (k ProviderKind) UserFacingName() string {
	switch k {
	case HermesGateway:
		return "Hermes"
	case Anthropic:
		return "Anthropic"
	case OpenAI:
		return "OpenAI"
	case Gemini:
		return "Gemini"
	case Together:
		return "Together"
	case Groq:
		return "Groq"
	case Fireworks:
		return "Fireworks"
	case DeepInfra:
		return "DeepInfra"
	case DeepseekDirect, Deepseek:
		return "DeepSeek"
	case OpenRouter:
		return "OpenRouter"
	case Kimi:
		return "Kimi"
	case Ollama:
		return "Ollama"
	case XAI:
		return "Grok"
	case Nvidia:
		return "NVIDIA NIM"
	case Nous:
		return "Nous"
	case Custom:
		return "Custom"
	}
	return string(k)
}

// CredentialMode is Swift's LLMBrainMode: who pays. Empty means nobody declared
// an intent (internal or cron work with no user) and keeps managed semantics.
type CredentialMode string

const (
	ModeUnset   CredentialMode = ""
	ModeManaged CredentialMode = "managed"
	ModeBYOK    CredentialMode = "byok"
)
