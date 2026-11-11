package mcp

import (
	"strings"
)

// SetupDTO is AgentConnectionSetupDTO: nil fields are left out.
type SetupDTO struct {
	Kind        string  `json:"kind"`
	URL         string  `json:"url"`
	Token       string  `json:"token"`
	ConfigLabel string  `json:"configLabel"`
	ConfigLang  string  `json:"configLang"`
	Config      string  `json:"config"`
	SafeLabel   *string `json:"safeLabel,omitempty"`
	SafeLang    *string `json:"safeLang,omitempty"`
	Safe        *string `json:"safe,omitempty"`
	Export      *string `json:"export,omitempty"`
	SafeNote    *string `json:"safeNote,omitempty"`
	Prompt      string  `json:"prompt"`
}

// MCPSetup constants.
const (
	setupEnvVar     = "LUMINA_MCP_TOKEN"
	setupServerName = "luminavault"
)

func ptr(s string) *string { return &s }

// mcpURL trims "/" from both ends of the base, as CharacterSet trimming does.
func mcpURL(publicBaseURL string) string {
	return strings.Trim(publicBaseURL, "/") + "/v1/mcp"
}

// setupInstructions is MCPSetup.instructions.
func setupInstructions(kind, publicBaseURL, token string) SetupDTO {
	url := mcpURL(publicBaseURL)
	setup := SetupDTO{
		Kind: kind, URL: url, Token: token,
		ConfigLabel: "connection details", ConfigLang: "text",
		Config: "Transport: streamable HTTP\n" +
			"URL:       " + url + "\n" +
			"Header:    Authorization: Bearer " + token,
		Prompt: setupPrompt(kind, url, token),
	}
	switch kind {
	case KindClaudeCode:
		setup.ConfigLabel, setup.ConfigLang = "one command", "bash"
		setup.Config = "claude mcp add --transport http " + setupServerName + " " + url + " \\\n" +
			"  --header \"Authorization: Bearer " + token + "\""
		setup.SafeLabel, setup.SafeLang = ptr(".mcp.json"), ptr("json")
		setup.Safe = ptr("{\n" +
			"  \"mcpServers\": {\n" +
			"    \"" + setupServerName + "\": {\n" +
			"      \"type\": \"http\",\n" +
			"      \"url\": \"" + url + "\",\n" +
			"      \"headers\": {\n" +
			"        \"Authorization\": \"Bearer ${" + setupEnvVar + "}\"\n" +
			"      }\n" +
			"    }\n" +
			"  }\n" +
			"}")
		setup.Export = ptr("export " + setupEnvVar + "=" + token)
		setup.SafeNote = ptr("If that file lives in a git repository, use this version instead and keep the key in your environment — .mcp.json is committed more often than people expect.")
	case KindCodex:
		setup.ConfigLabel, setup.ConfigLang = "one command", "bash"
		setup.Config = "codex mcp add " + setupServerName + " --url " + url + " --bearer-token-env-var " + setupEnvVar
		setup.SafeLabel, setup.SafeLang = ptr("~/.codex/config.toml"), ptr("toml")
		setup.Safe = ptr("[mcp_servers." + setupServerName + "]\n" +
			"url = \"" + url + "\"\n" +
			"bearer_token_env_var = \"" + setupEnvVar + "\"")
		setup.Export = ptr("export " + setupEnvVar + "=" + token)
		setup.SafeNote = ptr("That command writes this, which you can also write yourself. Codex reads the key from the environment, so the file never contains it.")
	case KindHermes:
		setup.ConfigLabel, setup.ConfigLang = "shell", "bash"
		setup.Config = "hermes mcp add " + setupServerName + " --url " + url + " --auth header\n" +
			"# When prompted for the header:\n" +
			"# Authorization: Bearer " + token
	}
	return setup
}

func setupPreview(kind, publicBaseURL string) SetupDTO {
	return setupInstructions(kind, publicBaseURL, PlaceholderToken)
}

func setupPrompt(kind, url, token string) string {
	return strings.Join([]string{
		"Add an MCP server called \"" + setupServerName + "\" to your own configuration, then confirm it works.",
		"",
		"  Transport: streamable HTTP",
		"  URL:       " + url,
		"  Header:    Authorization: Bearer " + token,
		"",
		"Put the credential in a header, never in the URL — URLs end up in logs. Do not register LuminaVault a second time under another name; duplicate tool names across MCP servers make tool selection unpredictable.",
		"",
		"Then verify by listing the tools: you should see status, search, browse, read, recent, links, context, index, memory_save, memory_update and memory_forget, plus the health, calendar and reminders tools. Report what you found. Do not call `index` or any memory tool as part of this check — they write.",
		"",
		"Once connected: every call acts as one account — mine. Ask before running `index`, and when you use a passage from `search`, quote the path and line range it gave you rather than inventing one.",
		"",
		"Finally, add the block below to " + instructionsHome(kind) + ", so you use LuminaVault without being asked each session. Keep everything already there; if a \"LuminaVault memory\" section exists, replace only that section. Show me the change before you save it.",
		"",
		memoryHabit,
	}, "\n")
}

func instructionsHome(kind string) string {
	switch kind {
	case KindClaudeCode:
		return "my CLAUDE.md (~/.claude/CLAUDE.md for every project)"
	case KindCodex:
		return "my AGENTS.md (~/.codex/AGENTS.md for every project)"
	case KindHermes:
		return "your persistent memory or instructions, the place you read at the start of every session"
	default:
		return "the instructions file or setting you read at the start of every session"
	}
}

var memoryHabit = strings.Join([]string{
	"## LuminaVault memory",
	"LuminaVault (MCP server \"" + setupServerName + "\") is my long-term memory across sessions and tools.",
	"- At the start of a session, call `memory_context` for my SOUL.md and recent decisions and preferences.",
	"- Before substantial work, `search` it for my preferences, decisions and context on the topic.",
	"- Search before saving. If a memory already says it, correct it with `memory_update` instead of saving another.",
	"- Save with `memory_save`: decisions I confirmed, durable preferences, and facts about me or my work that will matter next time. One plain statement per memory, with its `kind` (preference, decision or fact), filed with `space` when one fits.",
	"- Do not save secrets or credentials, passing remarks, your own guesses, or anything I ask you not to keep.",
	"- Use `memory_forget` only after I agree.",
	"- My current instructions beat anything stored. Say when an answer relies on a memory.",
	"- If a save is refused because this key is read-only, carry on and tell me what you would have saved.",
}, "\n")
