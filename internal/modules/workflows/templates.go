package workflows

import (
	"lumina/internal/wire"

	"github.com/google/uuid"
)

// templateCatalog is WorkflowTemplateCatalog.templates. Edge ids are random and
// fixed for the life of the process, as Swift's `static let` makes them.
var templateCatalog = []WorkflowTemplateDTO{
	makeTemplate("research-brief", "Research Brief", "Search your vault, analyze the evidence, and produce a concise cited brief.", "Research", []WorkflowNodeDTO{
		catalogNode("10000000-0000-0000-0000-000000000001", Trigger, "Start", 80, nil),
		catalogNode("10000000-0000-0000-0000-000000000002", MemorySearch, "Find evidence", 340, StringMap{"query": "{{topic}}", "limit": "8"}),
		catalogNode("10000000-0000-0000-0000-000000000003", Llm, "Write brief", 600, StringMap{"prompt": "Create a structured research brief about {{topic}} using only this vault evidence:\n\n{{nodes.10000000-0000-0000-0000-000000000002.text}}"}),
		catalogNode("10000000-0000-0000-0000-000000000004", Output, "Result", 860, StringMap{"value": "{{nodes.10000000-0000-0000-0000-000000000003.text}}"}),
	}),
	makeTemplate("memory-digest", "Memory Digest", "Turn a focused memory search into a practical digest.", "Memory", []WorkflowNodeDTO{
		catalogNode("20000000-0000-0000-0000-000000000001", Trigger, "Start", 80, nil),
		catalogNode("20000000-0000-0000-0000-000000000002", MemorySearch, "Recall", 340, StringMap{"query": "{{topic}}", "limit": "12"}),
		catalogNode("20000000-0000-0000-0000-000000000003", Llm, "Summarize", 600, StringMap{"prompt": "Summarize the following memories into themes, decisions, and next actions:\n\n{{nodes.20000000-0000-0000-0000-000000000002.text}}"}),
		catalogNode("20000000-0000-0000-0000-000000000004", Output, "Digest", 860, StringMap{"value": "{{nodes.20000000-0000-0000-0000-000000000003.text}}"}),
	}),
	makeTemplate("extract-review-save", "Extract, Review & Save", "Extract durable knowledge, pause for your review, then save it to memory.", "Knowledge", []WorkflowNodeDTO{
		catalogNode("30000000-0000-0000-0000-000000000001", Trigger, "Start", 80, nil),
		catalogNode("30000000-0000-0000-0000-000000000002", Llm, "Extract knowledge", 340, StringMap{"prompt": "Extract the durable facts, decisions, and follow-ups from:\n\n{{source}}"}),
		catalogNode("30000000-0000-0000-0000-000000000003", Approval, "Review extraction", 600, StringMap{"message": "Review the extracted knowledge. You can attach memories before continuing."}),
		catalogNode("30000000-0000-0000-0000-000000000004", MemoryWrite, "Save memory", 860, StringMap{"content": "{{nodes.30000000-0000-0000-0000-000000000002.text}}"}),
		catalogNode("30000000-0000-0000-0000-000000000005", Output, "Saved", 1120, StringMap{"value": "{{nodes.30000000-0000-0000-0000-000000000004.text}}"}),
	}),
	makeTemplate("sequential-multi-model-synthesis", "Sequential Multi-Model Synthesis", "Ask two models in sequence, then synthesize their perspectives.", "Multi-model", []WorkflowNodeDTO{
		catalogNode("40000000-0000-0000-0000-000000000001", Trigger, "Start", 80, nil),
		catalogNode("40000000-0000-0000-0000-000000000002", Llm, "First analysis", 340, StringMap{"prompt": "Analyze this question carefully:\n\n{{question}}", "model": "auto"}),
		catalogNode("40000000-0000-0000-0000-000000000003", Llm, "Second perspective", 600, StringMap{"prompt": "Offer an independent critical perspective on {{question}}. Consider this prior analysis:\n\n{{nodes.40000000-0000-0000-0000-000000000002.text}}", "model": "auto"}),
		catalogNode("40000000-0000-0000-0000-000000000004", Llm, "Synthesize", 860, StringMap{"prompt": "Synthesize a final answer from these two analyses:\n\nA: {{nodes.40000000-0000-0000-0000-000000000002.text}}\n\nB: {{nodes.40000000-0000-0000-0000-000000000003.text}}", "model": "auto"}),
		catalogNode("40000000-0000-0000-0000-000000000005", Output, "Result", 1120, StringMap{"value": "{{nodes.40000000-0000-0000-0000-000000000004.text}}"}),
	}),
}

func templateByID(id string) (WorkflowTemplateDTO, bool) {
	for _, t := range templateCatalog {
		if t.Id == id {
			return t, true
		}
	}
	return WorkflowTemplateDTO{}, false
}

func makeTemplate(id, name, description, category string, nodes []WorkflowNodeDTO) WorkflowTemplateDTO {
	edges := make([]WorkflowEdgeDTO, 0, len(nodes)-1)
	for i := range len(nodes) - 1 {
		edges = append(edges, WorkflowEdgeDTO{
			Id:           wire.UUID{UUID: uuid.New()},
			SourceNodeID: nodes[i].Id,
			SourcePort:   "output",
			TargetNodeID: nodes[i+1].Id,
			TargetPort:   "input",
		})
	}
	version := 1
	return WorkflowTemplateDTO{
		Id:              id,
		Name:            name,
		DescriptionText: description,
		Category:        category,
		Definition:      WorkflowDefinitionDTO{SchemaVersion: &version, Trigger: Manual, TriggerConfiguration: StringMap{}, Nodes: nodes, Edges: edges},
	}
}

// catalogNode lays every node out on the catalog's one row, y = 180.
func catalogNode(id string, kind WorkflowNodeKind, name string, x wire.Double, config StringMap) WorkflowNodeDTO {
	if config == nil {
		config = StringMap{}
	}
	return WorkflowNodeDTO{Id: wire.UUID{UUID: uuid.MustParse(id)}, Kind: kind, Name: name, X: x, Y: 180, Configuration: config}
}
