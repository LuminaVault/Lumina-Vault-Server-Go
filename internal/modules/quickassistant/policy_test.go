package quickassistant

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func fields(s string) []string { return strings.Fields(s) }

var (
	runTools     = fields("search browse read recent memory_context health_query calendar_query app_reminders_list reminders_list memory_save app_reminder_create")
	confirmTools = fields("calendar_create memory_forget")
	clientTools  = fields("open_screen")
	// MCPToolCatalog.all, in catalog order.
	catalogNames = fields("status search browse read recent links context index memory_context memory_save memory_update memory_forget health_query calendar_query reminders_list calendar_create reminder_create")
)

func names(defs []Tool) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func find(t *testing.T, defs []Tool, name string) Tool {
	t.Helper()
	for _, d := range defs {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("tool %s not offered", name)
	return Tool{}
}

func TestPolicyTable(t *testing.T) {
	for _, n := range runTools {
		require.Equal(t, Run, ActionFor(n), n)
	}
	for _, n := range confirmTools {
		require.Equal(t, Confirm, ActionFor(n), n)
	}
	for _, n := range clientTools {
		require.Equal(t, Client, ActionFor(n), n)
	}
	for _, n := range fields("memory_update index reminder_create status links context nope") {
		require.Equal(t, Refused, ActionFor(n), n)
	}
}

func TestEveryCatalogToolIsClassifiedDeliberately(t *testing.T) {
	require.Len(t, catalogNames, 17)
	for _, n := range catalogNames {
		switch {
		case slices.Contains(runTools, n):
			require.Equal(t, Run, ActionFor(n), n)
		case slices.Contains(confirmTools, n):
			require.Equal(t, Confirm, ActionFor(n), n)
		default:
			require.Equal(t, Refused, ActionFor(n), n)
		}
	}
}

func TestRefusedToolsAreNotOffered(t *testing.T) {
	for _, d := range Definitions(DefinitionOptions{DeviceWrites: true}) {
		require.NotEqual(t, Refused, ActionFor(d.Name), d.Name)
	}
}

func TestEveryNonRefusedToolIsOffered(t *testing.T) {
	want := slices.Concat(runTools, confirmTools, clientTools)
	got := names(Definitions(DefinitionOptions{DeviceWrites: true}))
	require.ElementsMatch(t, want, got)
	require.Len(t, got, len(want), "no duplicates")
	require.Equal(t,
		fields("search browse read recent memory_context memory_save memory_forget health_query calendar_query reminders_list calendar_create app_reminder_create app_reminders_list open_screen"),
		got, "catalog order, then the assistant-only tools")
}

func TestDefinitionsMatchSwiftGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/tools.golden.json")
	require.NoError(t, err)
	got, err := json.Marshal(Definitions(DefinitionOptions{DeviceWrites: true}))
	require.NoError(t, err)
	require.JSONEq(t, string(raw), string(got))
	// JSONEq ignores order; the order of the array is pinned above.
	var want, have []map[string]any
	require.NoError(t, json.Unmarshal(raw, &want))
	require.NoError(t, json.Unmarshal(got, &have))
	for i := range want {
		require.Equal(t, want[i]["name"], have[i]["name"])
	}
}

func TestLoopCalendarCreateReadsLocalTimes(t *testing.T) {
	tool := find(t, Definitions(DefinitionOptions{DeviceWrites: true}), "calendar_create")
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
	require.Equal(t, "Start, ISO 8601. With no offset it is read in the user's time zone.", schema.Properties["start"].Description)
	require.Equal(t, "End, ISO 8601. With no offset it is read in the user's time zone. Defaults to one hour after start.", schema.Properties["end"].Description)
	require.True(t, strings.HasPrefix(tool.Description, "Propose an event"))
}

func schemaOf(t *testing.T, tool Tool) (props map[string]any, required []string) {
	t.Helper()
	var s struct {
		Type                 string         `json:"type"`
		Properties           map[string]any `json:"properties"`
		Required             []string       `json:"required"`
		AdditionalProperties *bool          `json:"additionalProperties"`
	}
	require.NoError(t, json.Unmarshal(tool.InputSchema, &s))
	require.Equal(t, "object", s.Type)
	require.NotNil(t, s.AdditionalProperties)
	require.False(t, *s.AdditionalProperties, "strict schema")
	return s.Properties, s.Required
}

func TestOpenScreenOffersEveryScreenExceptUnknown(t *testing.T) {
	tool := find(t, Definitions(DefinitionOptions{DeviceWrites: true}), "open_screen")
	props, _ := schemaOf(t, tool)
	enum := props["screen"].(map[string]any)["enum"].([]any)
	got := make([]string, 0, len(enum))
	for _, e := range enum {
		got = append(got, e.(string))
	}
	require.Equal(t, fields("home spaces ai brain insights settings dataAccess brief learned memory"), got)
	require.NotContains(t, got, "unknown")
}

func TestNewToolsHaveStrictSchemasAndFlags(t *testing.T) {
	defs := Definitions(DefinitionOptions{DeviceWrites: true})

	create := find(t, defs, "app_reminder_create")
	require.True(t, create.Writes)
	require.False(t, create.Personal)
	props, req := schemaOf(t, create)
	require.Equal(t, []string{"title", "fireAt"}, req)
	require.Len(t, props, 3)

	list := find(t, defs, "app_reminders_list")
	require.False(t, list.Writes)
	props, req = schemaOf(t, list)
	require.Empty(t, props)
	require.Empty(t, req)

	open := find(t, defs, "open_screen")
	require.False(t, open.Writes)
	_, req = schemaOf(t, open)
	require.Equal(t, []string{"screen"}, req)
}

func TestCalendarCreateIsLeftOutWithoutDeviceWrites(t *testing.T) {
	got := names(Definitions(DefinitionOptions{}))
	require.NotContains(t, got, "calendar_create")
	require.Len(t, got, 13)
	require.Equal(t, Confirm, ActionFor("calendar_create"))
}

func TestReminderToolsArePersonalAndOpenScreenIsNot(t *testing.T) {
	require.True(t, TouchesPersonalData("app_reminders_list"))
	require.True(t, TouchesPersonalData("app_reminder_create"))
	for _, n := range fields("health_query calendar_query reminders_list calendar_create reminder_create") {
		require.True(t, TouchesPersonalData(n), n)
	}
	for _, n := range fields("open_screen search memory_save memory_forget nope") {
		require.False(t, TouchesPersonalData(n), n)
	}
}

func TestIsWrite(t *testing.T) {
	for _, n := range fields("memory_save memory_forget calendar_create app_reminder_create") {
		require.True(t, IsWrite(n), n)
	}
	for _, n := range fields("search read open_screen app_reminders_list health_query nope") {
		require.False(t, IsWrite(n), n)
	}
	for _, d := range Definitions(DefinitionOptions{DeviceWrites: true}) {
		require.Equal(t, d.Writes, IsWrite(d.Name), d.Name)
	}
}

func TestOpenAITools(t *testing.T) {
	defs := Definitions(DefinitionOptions{})
	got, err := json.Marshal(OpenAITools(defs[:1]))
	require.NoError(t, err)
	want, err := json.Marshal([]map[string]any{{"type": "function", "function": map[string]any{
		"name": defs[0].Name, "description": defs[0].Description, "parameters": json.RawMessage(defs[0].InputSchema),
	}}})
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got))
	require.Len(t, OpenAITools(defs), len(defs))
}
