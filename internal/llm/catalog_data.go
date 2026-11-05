package llm

// routerCatalogEntries is RouterModelCatalog.entries, in order. Routing
// estimates, not invoices: price changes are reviewed and shipped in code.
//
// Zero-cost ":free" slugs do not belong here. Every entry is expanded into the
// Auto pool, so a $0 entry wins cost-first scoring and hands a paying user a
// rate-limited free model while burning the account-wide allowance.
var routerCatalogEntries = []RouterCatalogEntry{
	{Provider: "anthropic", Model: "claude-3-5-haiku-20241022", DisplayName: "Claude 3.5 Haiku", TaskQuality: ratings(78, 72, 74, 60, 80), InputPerMillionUsdMicros: 800000, OutputPerMillionUsdMicros: 4000000, DefaultLatencyMs: 600, Capabilities: []string{"chat", "tools"}, Tier: TierFast},
	{Provider: "gemini", Model: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash", TaskQuality: ratings(82, 78, 76, 85, 84), InputPerMillionUsdMicros: 150000, OutputPerMillionUsdMicros: 600000, DefaultLatencyMs: 500, Capabilities: []string{"chat", "tools", "search"}, Tier: TierFast},
	{Provider: "gemini", Model: "gemini-2.5-flash-lite", DisplayName: "Gemini 2.5 Flash Lite", TaskQuality: ratings(74, 68, 70, 72, 76), InputPerMillionUsdMicros: 75000, OutputPerMillionUsdMicros: 300000, DefaultLatencyMs: 400, Capabilities: []string{"chat"}, Tier: TierFast},
	{Provider: "openai", Model: "gpt-4o-mini", DisplayName: "GPT-4o mini", TaskQuality: ratings(80, 76, 78, 70, 82), InputPerMillionUsdMicros: 150000, OutputPerMillionUsdMicros: 600000, DefaultLatencyMs: 550, Capabilities: []string{"chat", "tools"}, Tier: TierFast},
	{Provider: "openRouter", Model: ManagedModel, DisplayName: "DeepSeek V4 Flash", TaskQuality: ratings(88, 90, 91, 65, 88), InputPerMillionUsdMicros: 90000, OutputPerMillionUsdMicros: 180000, DefaultLatencyMs: 600, Capabilities: []string{"chat", "tools", "reasoning"}, Tier: TierFast},
	{Provider: "xai", Model: "grok-3-mini", DisplayName: "Grok 3 mini", TaskQuality: ratings(78, 76, 74, 80, 76), InputPerMillionUsdMicros: 300000, OutputPerMillionUsdMicros: 500000, DefaultLatencyMs: 700, Capabilities: []string{"chat", "tools"}, Tier: TierFast},
	{Provider: "nvidia", Model: "nvidia/nemotron-3-nano-30b-a3b", DisplayName: "Nemotron 3 Nano 30B (NVIDIA)", TaskQuality: ratings(72, 70, 70, 58, 74), InputPerMillionUsdMicros: 50000, OutputPerMillionUsdMicros: 200000, DefaultLatencyMs: 450, Capabilities: []string{"chat", "tools"}, Tier: TierFast},
	{Provider: "openRouter", Model: "qwen/qwen-2.5-72b-instruct", DisplayName: "Qwen 2.5 72B", TaskQuality: ratings(82, 84, 78, 65, 83), InputPerMillionUsdMicros: 350000, OutputPerMillionUsdMicros: 400000, DefaultLatencyMs: 1100, Capabilities: []string{"chat", "tools"}, Tier: TierBalanced},
	{Provider: "anthropic", Model: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", TaskQuality: ratings(94, 96, 95, 76, 96), InputPerMillionUsdMicros: 3000000, OutputPerMillionUsdMicros: 15000000, DefaultLatencyMs: 1500, Capabilities: []string{"chat", "tools", "reasoning"}, Tier: TierBalanced},
	{Provider: "openai", Model: "gpt-4o", DisplayName: "GPT-4o", TaskQuality: ratings(90, 88, 90, 78, 90), InputPerMillionUsdMicros: 2500000, OutputPerMillionUsdMicros: 10000000, DefaultLatencyMs: 1200, Capabilities: []string{"chat", "tools"}, Tier: TierBalanced},
	{Provider: "gemini", Model: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", TaskQuality: ratings(91, 92, 89, 90, 92), InputPerMillionUsdMicros: 1250000, OutputPerMillionUsdMicros: 10000000, DefaultLatencyMs: 1250, Capabilities: []string{"chat", "tools", "search"}, Tier: TierBalanced},
	{Provider: "xai", Model: "grok-4", DisplayName: "Grok 4", TaskQuality: ratings(91, 92, 88, 96, 88), InputPerMillionUsdMicros: 3000000, OutputPerMillionUsdMicros: 15000000, DefaultLatencyMs: 1350, Capabilities: []string{"chat", "tools", "search"}, Tier: TierMax},
	{Provider: "nvidia", Model: "meta/llama-3.3-70b-instruct", DisplayName: "Llama 3.3 70B (NVIDIA)", TaskQuality: ratings(80, 78, 77, 60, 82), InputPerMillionUsdMicros: 600000, OutputPerMillionUsdMicros: 600000, DefaultLatencyMs: 850, Capabilities: []string{"chat", "tools"}, Tier: TierBalanced},
	{Provider: "nvidia", Model: "nvidia/nemotron-3-super-120b-a12b", DisplayName: "Nemotron 3 Super 120B", TaskQuality: ratings(88, 90, 91, 62, 86), InputPerMillionUsdMicros: 85000, OutputPerMillionUsdMicros: 400000, DefaultLatencyMs: 900, Capabilities: []string{"chat", "tools", "reasoning"}, Tier: TierBalanced},
	{Provider: "openRouter", Model: "anthropic/claude-sonnet-4.6", DisplayName: "Claude Sonnet 4.6 (OpenRouter)", TaskQuality: ratings(94, 96, 95, 76, 96), InputPerMillionUsdMicros: 3000000, OutputPerMillionUsdMicros: 15000000, DefaultLatencyMs: 1550, Capabilities: []string{"chat", "tools", "reasoning"}, Tier: TierBalanced},
	{Provider: "openRouter", Model: "x-ai/grok-4.5", DisplayName: "Grok 4.5 (OpenRouter)", TaskQuality: ratings(91, 92, 88, 96, 88), InputPerMillionUsdMicros: 2000000, OutputPerMillionUsdMicros: 6000000, DefaultLatencyMs: 1400, Capabilities: []string{"chat", "tools", "search"}, Tier: TierMax},
	{Provider: "openRouter", Model: "openai/gpt-5", DisplayName: "GPT-5 (OpenRouter)", TaskQuality: ratings(95, 96, 96, 80, 94), InputPerMillionUsdMicros: 1250000, OutputPerMillionUsdMicros: 10000000, DefaultLatencyMs: 1750, Capabilities: []string{"chat", "tools", "reasoning"}, Tier: TierMax},
	{Provider: "openRouter", Model: "anthropic/claude-opus-4.1", DisplayName: "Claude Opus 4.1 (OpenRouter)", TaskQuality: ratings(97, 98, 97, 78, 97), InputPerMillionUsdMicros: 15000000, OutputPerMillionUsdMicros: 75000000, DefaultLatencyMs: 2250, Capabilities: []string{"chat", "tools", "reasoning"}, Tier: TierMax},
	{Provider: "openai", Model: "gpt-5", DisplayName: "GPT-5", TaskQuality: ratings(95, 96, 94, 80, 94), InputPerMillionUsdMicros: 1250000, OutputPerMillionUsdMicros: 10000000, DefaultLatencyMs: 1700, Capabilities: []string{"chat", "tools", "reasoning"}, Tier: TierMax},
	{Provider: "anthropic", Model: "claude-opus-4-1", DisplayName: "Claude Opus 4.1", TaskQuality: ratings(97, 98, 96, 78, 97), InputPerMillionUsdMicros: 15000000, OutputPerMillionUsdMicros: 75000000, DefaultLatencyMs: 2200, Capabilities: []string{"chat", "tools", "reasoning"}, Tier: TierMax},
	{Provider: "openai", Model: "o3", DisplayName: "o3 (reasoning)", TaskQuality: ratings(94, 98, 95, 70, 90), InputPerMillionUsdMicros: 10000000, OutputPerMillionUsdMicros: 40000000, DefaultLatencyMs: 3000, Capabilities: []string{"chat", "reasoning"}, Tier: TierMax},
	{Provider: "nvidia", Model: "nvidia/nemotron-3-ultra-550b-a55b", DisplayName: "Nemotron 3 Ultra 550B (NVIDIA)", TaskQuality: ratings(92, 94, 93, 64, 90), InputPerMillionUsdMicros: 600000, OutputPerMillionUsdMicros: 3600000, DefaultLatencyMs: 1100, Capabilities: []string{"chat", "tools", "reasoning"}, Tier: TierMax},
}

// routerPricingOnlyEntries is RouterModelCatalog.pricingOnlyEntries: models the
// server can be charged for but that nothing may route to by choice. A row in
// the routable list would let any tenant pin the trial model.
var routerPricingOnlyEntries = []RouterCatalogEntry{
	{Provider: "anthropic", Model: "claude-haiku-5-5", DisplayName: "Claude Haiku 5.5", TaskQuality: ratings(84, 82, 82, 70, 86), InputPerMillionUsdMicros: 100000, OutputPerMillionUsdMicros: 500000, DefaultLatencyMs: 600, Capabilities: []string{"chat", "tools"}, Tier: TierFast},
}

// llmModelCatalog is LuminaVaultShared's LLMModelCatalog. Ollama and custom are
// empty: their model names are the tenant's own. A zero ContextWindow means
// unknown.
var llmModelCatalog = map[ProviderID][]LLMModelInfo{
	"gemini": {
		{ID: "gemini-2.5-flash-lite", DisplayName: "Gemini 2.5 Flash Lite", ContextWindow: 1000000, Tier: TierFast},
		{ID: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash", ContextWindow: 1000000, Tier: TierFast},
		{ID: "gemini-2.0-flash", DisplayName: "Gemini 2.0 Flash", ContextWindow: 1000000, Tier: TierFast},
		{ID: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", ContextWindow: 1000000, Tier: TierBalanced},
		{ID: "gemini-3-flash-preview", DisplayName: "Gemini 3 Flash (preview)", ContextWindow: 1000000, Tier: TierFast},
		{ID: "gemini-3-pro-preview", DisplayName: "Gemini 3 Pro (preview)", ContextWindow: 1000000, Tier: TierMax},
	},
	"openai": {
		{ID: "gpt-4o-mini", DisplayName: "GPT-4o mini", ContextWindow: 128000, Tier: TierFast},
		{ID: "gpt-4.1-mini", DisplayName: "GPT-4.1 mini", ContextWindow: 1000000, Tier: TierFast},
		{ID: "gpt-4o", DisplayName: "GPT-4o", ContextWindow: 128000, Tier: TierBalanced},
		{ID: "gpt-4.1", DisplayName: "GPT-4.1", ContextWindow: 1000000, Tier: TierBalanced},
		{ID: "o4-mini", DisplayName: "o4-mini (reasoning)", ContextWindow: 200000, Tier: TierBalanced},
		{ID: "o3", DisplayName: "o3 (reasoning)", ContextWindow: 200000, Tier: TierMax},
	},
	"anthropic": {
		{ID: "claude-3-5-haiku-20241022", DisplayName: "Claude 3.5 Haiku", ContextWindow: 200000, Tier: TierFast},
		{ID: "claude-sonnet-4-5", DisplayName: "Claude Sonnet 4.5", ContextWindow: 200000, Tier: TierBalanced},
		{ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude 3.5 Sonnet", ContextWindow: 200000, Tier: TierBalanced},
		{ID: "claude-opus-4-1", DisplayName: "Claude Opus 4.1", ContextWindow: 200000, Tier: TierMax},
	},
	"openRouter": {
		{ID: "openrouter/auto", DisplayName: "OpenRouter Auto (provider)", ContextWindow: 0, Tier: TierBalanced},
		{ID: "deepseek/deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash", ContextWindow: 1000000, Tier: TierFast},
		{ID: "deepseek/deepseek-chat", DisplayName: "DeepSeek Chat", ContextWindow: 64000, Tier: TierFast},
		{ID: "google/gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash", ContextWindow: 1000000, Tier: TierFast},
		{ID: "anthropic/claude-sonnet-5", DisplayName: "Claude Sonnet 5", ContextWindow: 200000, Tier: TierBalanced},
		{ID: "qwen/qwen-2.5-72b-instruct", DisplayName: "Qwen 2.5 72B", ContextWindow: 131072, Tier: TierBalanced},
		{ID: "openai/gpt-4o", DisplayName: "GPT-4o", ContextWindow: 128000, Tier: TierBalanced},
		{ID: "anthropic/claude-opus-4.8", DisplayName: "Claude Opus 4.8", ContextWindow: 200000, Tier: TierBalanced},
		{ID: "google/gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", ContextWindow: 1000000, Tier: TierBalanced},
		{ID: "anthropic/claude-opus-4", DisplayName: "Claude Opus 4", ContextWindow: 200000, Tier: TierMax},
		{ID: "anthropic/claude-opus-4.1", DisplayName: "Claude Opus 4.1", ContextWindow: 200000, Tier: TierMax},
		{ID: "openai/gpt-5", DisplayName: "GPT-5", ContextWindow: 400000, Tier: TierMax},
		{ID: "x-ai/grok-4.5", DisplayName: "Grok 4.5", ContextWindow: 256000, Tier: TierMax},
		{ID: "z-ai/glm-5.2:free", DisplayName: "GLM 5.2 (free)", ContextWindow: 256000, Tier: TierBalanced},
		{ID: "nvidia/nemotron-3-ultra-550b-a55b:free", DisplayName: "Nemotron 3 Ultra (free)", ContextWindow: 1000000, Tier: TierMax},
		{ID: "nvidia/nemotron-3-super-120b-a12b:free", DisplayName: "Nemotron 3 Super (free)", ContextWindow: 262144, Tier: TierBalanced},
		{ID: "nvidia/nemotron-3-nano-30b-a3b:free", DisplayName: "Nemotron 3 Nano (free)", ContextWindow: 256000, Tier: TierFast},
	},
	"xai": {
		{ID: "grok-3-mini", DisplayName: "Grok 3 mini", ContextWindow: 131072, Tier: TierFast},
		{ID: "grok-3", DisplayName: "Grok 3", ContextWindow: 131072, Tier: TierBalanced},
		{ID: "grok-4", DisplayName: "Grok 4", ContextWindow: 256000, Tier: TierMax},
	},
	"nvidia": {
		{ID: "nvidia/nemotron-3-nano-30b-a3b", DisplayName: "Nemotron 3 Nano 30B", ContextWindow: 262144, Tier: TierFast},
		{ID: "meta/llama-3.3-70b-instruct", DisplayName: "Llama 3.3 70B", ContextWindow: 128000, Tier: TierBalanced},
		{ID: "nvidia/nemotron-3-super-120b-a12b", DisplayName: "Nemotron 3 Super 120B", ContextWindow: 1000000, Tier: TierBalanced},
		{ID: "nvidia/nemotron-3-ultra-550b-a55b", DisplayName: "Nemotron 3 Ultra 550B", ContextWindow: 512288, Tier: TierMax},
		{ID: "deepseek-ai/deepseek-r1", DisplayName: "DeepSeek R1 (reasoning)", ContextWindow: 128000, Tier: TierMax},
	},
	"nous": {
		{ID: "stepfun/step-3.7-flash:free", DisplayName: "StepFun Step 3.7 Flash (free)", ContextWindow: 0, Tier: TierFast},
		{ID: "stepfun/step-3.7-flash", DisplayName: "StepFun Step 3.7 Flash", ContextWindow: 0, Tier: TierFast},
	},
}
