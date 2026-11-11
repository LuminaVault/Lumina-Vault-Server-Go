package mcp

import (
	"encoding/json"
	"lumina/internal/vaultindex"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Ports MCPContractTests and MCPSetupTests (Tests/AppTests/MCP).

func toolNames(filter func(tool) bool) []string {
	var out []string
	for _, t := range catalog {
		if filter(t) {
			out = append(out, t.name)
		}
	}
	sort.Strings(out)
	return out
}

func TestAdvertisedToolSetIsExactlySeventeen(t *testing.T) {
	require.Equal(t, []string{
		"browse", "calendar_create", "calendar_query", "context", "health_query", "index", "links",
		"memory_context", "memory_forget", "memory_save", "memory_update", "read", "recent",
		"reminder_create", "reminders_list", "search", "status",
	}, toolNames(func(tool) bool { return true }))
}

func TestOnlyIndexCreateAndMemoryToolsWrite(t *testing.T) {
	require.Equal(t, []string{"calendar_create", "index", "memory_forget", "memory_save", "memory_update", "reminder_create"},
		toolNames(func(t tool) bool { return t.writes }))
}

func TestPersonalToolsAreHealthCalendarReminders(t *testing.T) {
	require.Equal(t, []string{"calendar_create", "calendar_query", "health_query", "reminder_create", "reminders_list"},
		toolNames(func(t tool) bool { return t.personal }))
}

func TestRefusedPersonalCallBecomesToolError(t *testing.T) {
	r := personalResult(`{"status":"error","reason":"health access not allowed by the user"}`)
	require.Equal(t, true, r["isError"])
	require.Equal(t, "health access not allowed by the user", r["message"])
	ok := personalResult(`{"status":"ok","items":[]}`)
	require.Nil(t, ok["isError"])
	bad := personalResult(`not json`)
	require.Equal(t, map[string]any{"isError": true, "message": "tool returned no result"}, bad)
}

func TestEveryToolAdvertisesStrictObjectSchema(t *testing.T) {
	for _, tl := range catalog {
		s := tl.inputSchema()
		require.Equal(t, "object", s["type"], tl.name)
		require.Equal(t, false, s["additionalProperties"], tl.name)
		require.NotNil(t, s["properties"], tl.name)
		require.NotEmpty(t, tl.description, tl.name)
		for _, r := range tl.required {
			require.Contains(t, tl.properties, r, tl.name)
		}
	}
}

func TestToolsRequiringAnIdentifierDeclareIt(t *testing.T) {
	for name, want := range map[string]string{
		"read": "path", "links": "path", "context": "path", "search": "query",
		"memory_update": "memoryID", "memory_forget": "memoryID",
	} {
		tl, ok := lookupTool(name)
		require.True(t, ok)
		require.Contains(t, tl.required, want, name)
	}
}

func TestListingAnnotations(t *testing.T) {
	byName := map[string]map[string]any{}
	for _, raw := range toolListing() {
		l := raw.(map[string]any)
		byName[l["name"].(string)] = l["annotations"].(map[string]any)
	}
	require.Equal(t, true, byName["search"]["readOnlyHint"])
	require.Equal(t, false, byName["index"]["readOnlyHint"])
	require.Equal(t, true, byName["memory_update"]["destructiveHint"])
	require.Equal(t, true, byName["memory_forget"]["destructiveHint"])
	require.Equal(t, false, byName["memory_save"]["destructiveHint"])
	require.Equal(t, false, byName["calendar_create"]["idempotentHint"])
	require.Equal(t, true, byName["memory_save"]["idempotentHint"])
}

func TestUnknownToolLookupFails(t *testing.T) {
	_, ok := lookupTool("delete_everything")
	require.False(t, ok)
}

func TestLimitsValidate(t *testing.T) {
	v, err := validate(7, "limit", maxSearchLimit)
	require.NoError(t, err)
	require.Equal(t, 7, v)
	for _, bad := range []int{0, -1, maxSearchLimit + 1, 5000} {
		_, err := validate(bad, "limit", maxSearchLimit)
		require.Error(t, err)
		var rpcErr *rpcError
		require.ErrorAs(t, err, &rpcErr)
		require.Equal(t, -32602, rpcErr.Code)
	}
	_, err = validate(51, "limit", 50)
	require.EqualError(t, err, "limit must be between 1 and 50; got 51")
}

func TestContextSiblingBoundsMatchHTTP(t *testing.T) {
	tl, _ := lookupTool("context")
	s := tl.properties["siblingLimit"].(map[string]any)
	require.Equal(t, vaultindex.MaxSiblingLimit, s["maximum"])
	require.Equal(t, vaultindex.DefaultSiblingLimit, s["default"])
}

func TestErrorCodesMatchJSONRPC(t *testing.T) {
	require.Equal(t, -32700, parseError("").Code)
	require.Equal(t, -32601, methodNotFound("").Code)
	require.Equal(t, -32602, invalidParams("").Code)
	require.Equal(t, -32603, internalError("").Code)
}

func TestRequestDecoding(t *testing.T) {
	r, err := decodeRequest([]byte(`{"jsonrpc":"2.0","id":1e2,"method":"ping","params":{"a":[1,true,null]}}`))
	require.NoError(t, err)
	require.Equal(t, "ping", r.method)
	b, err := json.Marshal(success(r.id, map[string]any{}))
	require.NoError(t, err)
	require.JSONEq(t, `{"jsonrpc":"2.0","id":100,"result":{}}`, string(b))

	r, err = decodeRequest([]byte(`{"id":null,"method":"notifications/initialized"}`))
	require.NoError(t, err)
	require.Nil(t, r.id)

	for _, bad := range []string{``, `[]`, `{"method":7}`, `{"jsonrpc":2,"method":"ping"}`, `{"id":1}`, `{"method":"ping"} {}`} {
		_, err := decodeRequest([]byte(bad))
		require.Error(t, err, bad)
	}
}

func TestTypedAccessorsReturnNothingForTheWrongShape(t *testing.T) {
	args := map[string]any{"s": "x", "n": 2.9, "b": true}
	_, ok := argInt(args, "s")
	require.False(t, ok)
	n, ok := argInt(args, "n")
	require.True(t, ok)
	require.Equal(t, 2, n)
	_, ok = argString(args, "n")
	require.False(t, ok)
	require.Nil(t, argStringPtr(args, "b"))
}

func TestUnknownArgumentIsTheFirstSorted(t *testing.T) {
	tl, _ := lookupTool("search")
	name, ok := unknownArgument(tl, map[string]any{"query": "x", "zeta": 1, "alpha": 2})
	require.True(t, ok)
	require.Equal(t, "alpha", name)
}

func TestMCPURLIsBasePlusPath(t *testing.T) {
	require.Equal(t, "https://api.example.com/v1/mcp", mcpURL("https://api.example.com/"))
	require.Equal(t, "https://api.example.com/v1/mcp", mcpURL("https://api.example.com"))
}

func TestClaudeCodeUsesHeaderAndGitSafeForm(t *testing.T) {
	s := setupInstructions(KindClaudeCode, "https://x.test", "lv_secret")
	require.Contains(t, s.Config, `--header "Authorization: Bearer lv_secret"`)
	require.NotContains(t, s.URL, "lv_secret")
	require.NotNil(t, s.Safe)
	require.NotContains(t, *s.Safe, "lv_secret")
	require.Contains(t, *s.Safe, "${LUMINA_MCP_TOKEN}")
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(*s.Safe), &parsed))
}

func TestCodexNamesTheEnvVar(t *testing.T) {
	s := setupInstructions(KindCodex, "https://x.test", "lv_secret")
	require.Contains(t, s.Config, "--bearer-token-env-var LUMINA_MCP_TOKEN")
	require.NotContains(t, s.Config, "lv_secret")
}

func TestHermesUsesHeaderPrompt(t *testing.T) {
	s := setupInstructions(KindHermes, "https://x.test", "lv_secret")
	require.Contains(t, s.Config, "--auth header")
	require.Contains(t, s.Config, "Authorization: Bearer lv_secret")
}

func TestEveryPromptInstallsTheHabitWhereTheClientReadsIt(t *testing.T) {
	for _, k := range []string{KindClaudeCode, KindCodex, KindHermes, KindOther} {
		p := setupInstructions(k, "https://x.test", "lv_x").Prompt
		require.Contains(t, p, memoryHabit)
		require.Contains(t, p, instructionsHome(k))
		require.Contains(t, p, "Do not call `index` or any memory tool as part of this check")
	}
	require.NotContains(t, strings.ToLower(memoryHabit), "edit soul")
}

func TestEveryToolNamedInPromptAndInstructionsExists(t *testing.T) {
	names := toolNames(func(tool) bool { return true })
	text := setupPrompt(KindOther, "u", "t") + Instructions
	for _, word := range strings.FieldsFunc(text, func(r rune) bool { return r == '`' }) {
		if strings.Contains(word, "_") || slices.Contains([]string{"status", "search", "read", "links", "context", "index"}, word) {
			if strings.ContainsAny(word, " .") {
				continue
			}
			require.Contains(t, names, word)
		}
	}
}

func TestPreviewUsesTheLoudPlaceholder(t *testing.T) {
	s := setupPreview(KindClaudeCode, "https://x.test")
	require.Equal(t, PlaceholderToken, s.Token)
	require.False(t, strings.HasPrefix(s.Token, TokenPrefix))
}
