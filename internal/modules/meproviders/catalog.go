package meproviders

import "lumina/internal/llm"

func str(s string) *string { return &s }

// catalogEntry is ProviderCatalog.entry(for:). requiresAPIKey defaults to true
// and requiresBaseURL to false in ProviderCatalogEntryDTO's init.
func catalogEntry(p llm.ProviderID) catalogEntryDTO {
	e := catalogEntryDTO{Provider: p, RequiresAPIKey: true, Available: true}
	switch p {
	case "xai":
		e.DisplayName, e.DefaultBaseURL, e.KeyHint, e.KeysURL = "xAI", str("https://api.x.ai/v1"), str("xai-..."), str("https://console.x.ai")
	case "anthropic":
		e.DisplayName, e.DefaultBaseURL, e.KeyHint, e.KeysURL = "Anthropic", str("https://api.anthropic.com"), str("sk-ant-..."), str("https://console.anthropic.com/settings/keys")
	case "openai":
		e.DisplayName, e.DefaultBaseURL, e.KeyHint, e.KeysURL = "OpenAI", str("https://api.openai.com/v1"), str("sk-..."), str("https://platform.openai.com/api-keys")
	case "ollama":
		// A server the user runs, which commonly has no auth: the base URL is the credential.
		e.DisplayName, e.DefaultBaseURL, e.KeysURL = "Ollama", str("http://localhost:11434"), str("https://ollama.com/download")
		e.RequiresBaseURL, e.RequiresAPIKey = true, false
	case "openRouter":
		e.DisplayName, e.DefaultBaseURL, e.KeyHint, e.KeysURL = "OpenRouter", str("https://openrouter.ai/api/v1"), str("sk-or-v1-..."), str("https://openrouter.ai/keys")
	case "nvidia":
		e.DisplayName, e.DefaultBaseURL, e.KeyHint, e.KeysURL = "NVIDIA NIM", str("https://integrate.api.nvidia.com/v1"), str("nvapi-..."), str("https://build.nvidia.com")
	case "gemini":
		e.DisplayName, e.DefaultBaseURL, e.KeyHint, e.KeysURL = "Google Gemini", str("https://generativelanguage.googleapis.com"), str("AIza..."), str("https://aistudio.google.com/apikey")
	case "nous":
		e.DisplayName, e.DefaultBaseURL, e.KeysURL = "Nous Research", str("https://inference-api.nousresearch.com/v1"), str("https://portal.nousresearch.com")
	case "custom":
		// No default endpoint by definition, and often no auth.
		e.DisplayName = "Custom (OpenAI-compatible)"
		e.RequiresBaseURL, e.RequiresAPIKey = true, false
	}
	return e
}

// catalogAll is ProviderCatalog.all(available:).
func catalogAll(available func(llm.ProviderID) bool) []catalogEntryDTO {
	out := make([]catalogEntryDTO, len(providerIDs))
	for i, id := range providerIDs {
		out[i] = catalogEntry(id)
		out[i].Available = available(id)
	}
	return out
}

// defaultKind is ProvidersController.defaultKind(for:): the self-hosted two take
// a base URL, the rest a key.
func defaultKind(p llm.ProviderID) string {
	if p == "ollama" || p == "custom" {
		return string(kindHostURL)
	}
	return string(kindAPIKey)
}

// providerDisplayName is ConnectionsController.providerDisplayName. It is not the
// catalog's: custom reads "Custom Endpoint" here.
func providerDisplayName(p llm.ProviderID) string {
	switch p {
	case "xai":
		return "xAI"
	case "anthropic":
		return "Anthropic"
	case "openai":
		return "OpenAI"
	case "ollama":
		return "Ollama"
	case "openRouter":
		return "OpenRouter"
	case "nvidia":
		return "NVIDIA NIM"
	case "gemini":
		return "Google Gemini"
	case "nous":
		return "Nous Research"
	}
	return "Custom Endpoint"
}

// gateway is one HermesGatewayID with HermesGatewayCatalog's display name and
// description, in HermesGatewayID.allCases order.
type gateway struct{ id, name, description string }

var gateways = []gateway{
	{"telegram", "Telegram", "Chat with Lumina from Telegram via a bot you control. Send a voice message and it is transcribed automatically."},
	{"discord", "Discord", "Connect Lumina to a private Discord server."},
	{"slack", "Slack", "Pipe Lumina into a Slack workspace via your Slack app (Socket Mode)."},
	{"whatsapp", "WhatsApp", "Chat with Lumina on WhatsApp. Pairs by scanning a QR with your phone — no token to enter."},
	{"email", "Email", "Send and receive email as Lumina over IMAP/SMTP."},
	{"matrix", "Matrix", "Chat with Lumina over Matrix — your own homeserver or a public one like matrix.org."},
	{"ntfy", "ntfy", "Push-chat with Lumina via ntfy.sh (or a self-hosted server) — a topic is all you need."},
	{"mattermost", "Mattermost", "Connect Lumina to a Mattermost workspace via a bot account (outbound WebSocket)."},
	{"photon", "iMessage (Photon)", "Free iMessage access via Photon shared lines (the new free path). Device-code login + phone bind; contacts text the assigned number directly."},
}
