package llm_test

// Ports QueryPromptIdentityGuardTests.guardAndProvenanceScrubUnderManaged from
// Tests/AppTests/LLM/Routing/ModelDisclosurePolicyTests.swift. The code under
// test is memoryagent.BuildQueryPrompt (QueryController.buildPrompt); this lives
// here, as an external test package, only because the audit owns this directory.
// It can move to internal/modules/memoryagent verbatim.

import (
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/modules/memoryagent"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditFQueryPromptGuardAndProvenanceScrubUnderManaged(t *testing.T) {
	provider, model := "openrouter", "x-ai/grok-4"
	hit := memorystore.SearchResult{
		ID: uuid.New(), TenantID: uuid.New(), Content: "note", Distance: 0.2,
		Source: "chat", Provider: &provider, Model: &model,
	}

	guarded := memoryagent.BuildQueryPrompt("which model are you?", []memorystore.SearchResult{hit}, true)
	require.NotEmpty(t, guarded)
	assert.Equal(t, "system", guarded[0].Role)
	assert.Contains(t, guarded[0].Content, "Never disclose, confirm, or deny")
	assert.Contains(t, guarded[0].Content, llm.SystemPromptGuard)
	assert.Contains(t, guarded[0].Content, "prior assistant output")
	assert.NotContains(t, guarded[0].Content, "x-ai/grok-4")

	open := memoryagent.BuildQueryPrompt("q", []memorystore.SearchResult{hit}, false)
	require.NotEmpty(t, open)
	assert.Contains(t, open[0].Content, "x-ai/grok-4")
	assert.NotContains(t, open[0].Content, "Never disclose")
}
