package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// builtinSkillNames is SkillManifestTests.builtinSkillNames.
var builtinSkillNames = []string{
	"daily-brief", "kb-compile", "weekly-memo", "health-correlate", "capture-enrich",
	"belief-evolution", "pattern-detector", "contradiction-detector", "lapse-archiver",
}

func loadBuiltin(t *testing.T, name string) string {
	t.Helper()
	data, err := builtinFiles.ReadFile("builtin/" + name + "/SKILL.md")
	require.NoError(t, err)
	return string(data)
}

func requireManifestError(t *testing.T, err error, kind ManifestErrorKind, detail string) {
	t.Helper()
	var me *ManifestError
	require.True(t, errors.As(err, &me), "got %v", err)
	require.Equal(t, kind, me.Kind)
	require.Equal(t, detail, me.Detail)
}

// Swift SkillManifestTests, all 16.
func TestManifestBuiltins(t *testing.T) {
	t.Run("builtin resources are present and non empty", func(t *testing.T) {
		for _, name := range builtinSkillNames {
			contents := loadBuiltin(t, name)
			assert.True(t, strings.HasPrefix(contents, "---\n"), name)
			assert.Contains(t, contents, "name: "+name)
		}
	})
	t.Run("parser succeeds on every builtin manifest", func(t *testing.T) {
		for _, name := range builtinSkillNames {
			m, err := Parse(SourceBuiltin, loadBuiltin(t, name))
			require.NoError(t, err, name)
			assert.Equal(t, SourceBuiltin, m.Source)
			assert.Equal(t, name, m.Name)
			assert.NotEmpty(t, m.Description)
			assert.Contains(t, []Capability{CapabilityLow, CapabilityMedium, CapabilityHigh}, m.Capability)
			assert.NotContains(t, m.Body, "---\nname: "+name)
			assert.NotContains(t, m.Body, "metadata:")
		}
	})
	t.Run("every bundled top level skill parses with a name matching its directory", func(t *testing.T) {
		entries, err := builtinFiles.ReadDir("builtin")
		require.NoError(t, err)
		parsed := 0
		for _, e := range entries {
			m, err := Parse(SourceBuiltin, loadBuiltin(t, e.Name()))
			require.NoError(t, err, e.Name())
			assert.Equal(t, e.Name(), m.Name)
			parsed++
		}
		assert.Equal(t, 20, parsed, "the embed holds exactly the directories with a SKILL.md")
		assert.GreaterOrEqual(t, parsed, len(builtinSkillNames))
	})
	t.Run("placeholder skills are manual only", func(t *testing.T) {
		for _, name := range []string{"weekly-memo", "health-correlate"} {
			m, err := Parse(SourceBuiltin, loadBuiltin(t, name))
			require.NoError(t, err)
			assert.Nil(t, m.Schedule, name)
		}
		daily, err := Parse(SourceBuiltin, loadBuiltin(t, "daily-brief"))
		require.NoError(t, err)
		assert.NotNil(t, daily.Schedule)
	})
}

func TestManifestFields(t *testing.T) {
	t.Run("allowed-tools accepts space separated string", func(t *testing.T) {
		m, err := Parse(SourceBuiltin, `---
name: tool-string
description: Test allowed-tools as space-separated string.
allowed-tools: session_search vault_read memory_upsert
metadata:
  capability: low
---
body`)
		require.NoError(t, err)
		assert.Equal(t, []string{"session_search", "vault_read", "memory_upsert"}, m.AllowedTools)
	})
	t.Run("allowed-tools accepts yaml array", func(t *testing.T) {
		m, err := Parse(SourceBuiltin, `---
name: tool-array
description: Test allowed-tools as YAML array.
allowed-tools: [session_search, vault_read]
metadata:
  capability: medium
---
body`)
		require.NoError(t, err)
		assert.Equal(t, []string{"session_search", "vault_read"}, m.AllowedTools)
	})
	t.Run("outputs and daily run cap decode correctly", func(t *testing.T) {
		m, err := Parse(SourceVault, `---
name: full-meta
description: All optional metadata populated.
allowed-tools: session_search
metadata:
  capability: high
  schedule: "0 7 * * *"
  on_event: [vault_file_created, memory_upserted]
  outputs:
    - kind: memo
      path: memos/{date}/full.md
    - kind: apns_digest
    - kind: vault_rewrite
      category: reflection
  daily_run_cap:
    trial: 1
    pro: 3
    ultimate: 0
---
body`)
		require.NoError(t, err)
		assert.Equal(t, SourceVault, m.Source)
		assert.Equal(t, CapabilityHigh, m.Capability)
		assert.Equal(t, "0 7 * * *", *m.Schedule)
		assert.Equal(t, []string{"vault_file_created", "memory_upserted"}, m.OnEvent)
		require.Len(t, m.Outputs, 3)
		assert.Equal(t, OutputMemo, m.Outputs[0].Kind)
		assert.Equal(t, "memos/{date}/full.md", *m.Outputs[0].Path)
		assert.Equal(t, OutputAPNSDigest, m.Outputs[1].Kind)
		assert.Equal(t, OutputVaultRewrite, m.Outputs[2].Kind)
		assert.Equal(t, "reflection", *m.Outputs[2].Category)
		assert.Equal(t, &DailyRunCap{Trial: 1, Pro: 3, Ultimate: 0}, m.DailyRunCap)
	})
	t.Run("body strips frontmatter and trims surrounding whitespace", func(t *testing.T) {
		m, err := Parse(SourceBuiltin, `---
name: body-test
description: Body capture sanity.
metadata:
  capability: low
---


First body line.

Second body line.
`)
		require.NoError(t, err)
		assert.Equal(t, "First body line.\n\nSecond body line.", m.Body)
	})
	t.Run("a blank schedule parses as none", func(t *testing.T) {
		m, err := Parse(SourceBuiltin, `---
name: blank-schedule
description: Blank schedule means manual only.
allowed-tools: vault_read
metadata:
  capability: low
  # a comment line in the frontmatter is fine
  schedule: "  "
---
body`)
		require.NoError(t, err)
		assert.Nil(t, m.Schedule)
	})
}

func TestManifestRejections(t *testing.T) {
	t.Run("missing frontmatter is rejected", func(t *testing.T) {
		_, err := Parse(SourceBuiltin, "no frontmatter at all\njust body")
		requireManifestError(t, err, MissingFrontmatter, "")
	})
	t.Run("unclosed frontmatter is rejected with descriptive error", func(t *testing.T) {
		_, err := Parse(SourceBuiltin, "---\nname: dangling\ndescription: never closed")
		var me *ManifestError
		require.True(t, errors.As(err, &me))
		assert.Equal(t, MalformedFrontmatter, me.Kind)
		assert.Contains(t, me.Detail, "closing")
	})
	for _, tc := range []struct {
		name, yaml string
		kind       ManifestErrorKind
		detail     string
	}{
		{"missing required name is rejected", "description: no name field\nmetadata:\n  capability: low", MissingRequiredField, "name"},
		{"missing required description is rejected", "name: no-desc\nmetadata:\n  capability: low", MissingRequiredField, "description"},
		{"missing required capability is rejected", "name: no-cap\ndescription: missing metadata.capability", MissingRequiredField, "metadata.capability"},
		{"invalid capability value is rejected", "name: bad-cap\ndescription: d\nmetadata:\n  capability: extreme", InvalidCapability, "extreme"},
		{"invalid output kind is rejected", "name: bad-output\ndescription: d\nmetadata:\n  capability: low\n  outputs:\n    - kind: telegram_message", InvalidOutputKind, "telegram_message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(SourceBuiltin, "---\n"+tc.yaml+"\n---\nbody")
			requireManifestError(t, err, tc.kind, tc.detail)
		})
	}
}

// oracleCase is one row of testdata/manifest_oracle.json: what the real
// SkillManifestParser with Yams 5.4.0 said about a document. Rows named real/ are
// the 90 SKILL.md files under builtin/.
type oracleCase struct {
	ID           string              `json:"id"`
	Contents     *string             `json:"contents"`
	Error        string              `json:"error"`
	Detail       string              `json:"detail"`
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	AllowedTools []string            `json:"allowedTools"`
	Capability   string              `json:"capability"`
	Schedule     *string             `json:"schedule"`
	OnEvent      []string            `json:"onEvent"`
	Outputs      []map[string]string `json:"outputs"`
	Cap          []int               `json:"cap"`
	BodySHA256   string              `json:"bodySHA256"`
}

var oracleErrorKinds = map[string]ManifestErrorKind{
	"missing_frontmatter":    MissingFrontmatter,
	"malformed_frontmatter":  MalformedFrontmatter,
	"missing_required_field": MissingRequiredField,
	"invalid_capability":     InvalidCapability,
	"invalid_output_kind":    InvalidOutputKind,
}

func TestManifestMatchesSwiftOracle(t *testing.T) {
	raw, err := os.ReadFile("testdata/manifest_oracle.json")
	require.NoError(t, err)
	var cases []oracleCase
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.Greater(t, len(cases), 300)
	for _, tc := range cases {
		t.Run(tc.ID, func(t *testing.T) {
			var contents string
			if tc.Contents != nil {
				contents = *tc.Contents
			} else {
				data, err := os.ReadFile(filepath.Join("builtin", strings.TrimPrefix(tc.ID, "real/")))
				require.NoError(t, err)
				contents = string(data)
			}
			m, err := Parse(SourceBuiltin, contents)
			if tc.Error != "" {
				var me *ManifestError
				require.True(t, errors.As(err, &me), "Swift says %s %q, Go parsed %+v (err %v)", tc.Error, tc.Detail, m, err)
				assert.Equal(t, oracleErrorKinds[tc.Error], me.Kind, "%s", me)
				if tc.Error != "malformed_frontmatter" && tc.Error != "missing_frontmatter" {
					assert.Equal(t, tc.Detail, me.Detail)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.Name, m.Name)
			assert.Equal(t, tc.Description, m.Description)
			assert.Equal(t, tc.AllowedTools, m.AllowedTools)
			assert.Equal(t, tc.Capability, string(m.Capability))
			assert.Equal(t, tc.Schedule, m.Schedule)
			assert.Equal(t, tc.OnEvent, m.OnEvent)
			outputs := []map[string]string{}
			for _, o := range m.Outputs {
				row := map[string]string{"kind": string(o.Kind)}
				if o.Path != nil {
					row["path"] = *o.Path
				}
				if o.Category != nil {
					row["category"] = *o.Category
				}
				outputs = append(outputs, row)
			}
			assert.Equal(t, tc.Outputs, outputs)
			if tc.Cap == nil {
				assert.Nil(t, m.DailyRunCap)
			} else {
				assert.Equal(t, &DailyRunCap{Trial: tc.Cap[0], Pro: tc.Cap[1], Ultimate: tc.Cap[2]}, m.DailyRunCap)
			}
			sum := sha256.Sum256([]byte(m.Body))
			assert.Equal(t, tc.BodySHA256, hex.EncodeToString(sum[:]))
		})
	}
}

// Linux behaviour the macOS oracle can't show: components(separatedBy: "\n") doesn't
// split a CRLF there, so a CRLF file has no "---" line.
func TestManifestCRLFIsMissingFrontmatter(t *testing.T) {
	_, err := Parse(SourceBuiltin, "---\r\nname: x\r\ndescription: d\r\nmetadata:\r\n  capability: low\r\n---\r\nbody\r\n")
	requireManifestError(t, err, MissingFrontmatter, "")
}

// What Swift doesn't survive fails cleanly here.
func TestManifestPathologicalInput(t *testing.T) {
	t.Run("sexagesimal overflow, a trap in Swift", func(t *testing.T) {
		_, err := Parse(SourceBuiltin, "---\nname: x\ndescription: d\nmetadata:\n  capability: low\n  daily_run_cap: {trial: 1:1:1:1:1:1:1:1:1:1:1, pro: 1, ultimate: 1}\n---\n")
		requireManifestError(t, err, MalformedFrontmatter, "trial is not an integer")
	})
	t.Run("merge key bomb", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("---\nname: x\ndescription: d\na0: &a0 {k: v}\n")
		for i := 1; i < 40; i++ {
			fmt.Fprintf(&b, "a%d: &a%d {<<: [*a%d, *a%d]}\n", i, i, i-1, i-1)
		}
		b.WriteString("<<: *a39\n---\n")
		_, err := Parse(SourceBuiltin, b.String())
		requireManifestError(t, err, MalformedFrontmatter, "too many merged keys")
	})
}

// Swift DailyRunCapTests, all 4.
func TestDailyRunCap(t *testing.T) {
	limit := DailyRunCap{Trial: 3, Pro: 3, Ultimate: 0}
	t.Run("tier strings map to their declared caps", func(t *testing.T) {
		assert.Equal(t, 3, limit.Value("trial"))
		assert.Equal(t, 3, limit.Value("pro"))
		assert.Equal(t, 0, limit.Value("ultimate"))
	})
	t.Run("tier strings are case insensitive", func(t *testing.T) {
		assert.Equal(t, 0, limit.Value("ULTIMATE"))
		assert.Equal(t, 3, limit.Value("Pro"))
	})
	t.Run("unknown tier defaults to trial cap", func(t *testing.T) {
		assert.Equal(t, 3, limit.Value("enterprise"))
		assert.Equal(t, 3, limit.Value(""))
	})
	t.Run("zero ultimate cap encodes the unlimited contract", func(t *testing.T) {
		assert.Equal(t, 0, DailyRunCap{Trial: 1, Pro: 1, Ultimate: 0}.Value("ultimate"))
	})
}

// Swift MuseProactiveManifestTests: 3 of 4. The lock screen preview case is in
// internal/apns.
func TestMuseProactiveManifest(t *testing.T) {
	t.Run("chat_message is a manifest output kind", func(t *testing.T) {
		m, err := Parse(SourceVault, "---\nname: t\ndescription: t\nmetadata:\n  capability: low\n  outputs:\n    - kind: chat_message\n---\nbody")
		require.NoError(t, err)
		require.Len(t, m.Outputs, 1)
		assert.Equal(t, OutputChatMessage, m.Outputs[0].Kind)
	})
	t.Run("the morning brief posts into the chat and can read weather and the inbox", func(t *testing.T) {
		m, err := Parse(SourceBuiltin, loadBuiltin(t, "daily-brief"))
		require.NoError(t, err)
		require.Len(t, m.Outputs, 2)
		assert.Equal(t, OutputMemo, m.Outputs[0].Kind)
		assert.Equal(t, OutputChatMessage, m.Outputs[1].Kind)
		assert.Contains(t, m.AllowedTools, "weather_forecast")
		assert.Contains(t, m.AllowedTools, "mail_inbox_recent")
		assert.Contains(t, m.Body, "Inbox needs a look")
		assert.NotContains(t, m.Body, "TODO")
		assert.Equal(t, "0 7 * * *", *m.Schedule)
	})
	// JobAuthoring.skillMarkdown comes with 5E. This is its output for the Swift
	// test's arguments.
	t.Run("a new job answers in the chat thread", func(t *testing.T) {
		m, err := Parse(SourceVault, `---
name: job-dry-week
description: Dry week watch
license: MIT
allowed-tools: session_search vault_read weather_forecast mail_inbox_recent
metadata:
  capability: medium
  schedule: "0 8 * * *"
  on_event: []
  daily_run_cap: { trial: 1, pro: 6, ultimate: 24 }
  maxInputTokens: 8000
  outputs:
    - kind: chat_message
---
You are a LuminaVault job: "Dry week watch".
Domain: weather.
`)
		require.NoError(t, err)
		require.Len(t, m.Outputs, 1)
		assert.Equal(t, OutputChatMessage, m.Outputs[0].Kind)
		assert.Contains(t, m.AllowedTools, "weather_forecast")
		assert.Contains(t, m.AllowedTools, "mail_inbox_recent")
		assert.Contains(t, m.AllowedTools, "session_search")
	})
}
