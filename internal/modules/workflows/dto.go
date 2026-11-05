package workflows

import (
	"encoding/json"
	"lumina/internal/wire"
	"reflect"
	"slices"
)

// Request bodies, with fields in Swift's declaration order: wire.Bind reports the
// first failing key, and the generated models are alphabetical.

// definitionRequest is WorkflowDefinitionDTO's custom init(from:): schemaVersion
// defaults to 1 and triggerConfiguration to empty, also when null.
type definitionRequest struct {
	SchemaVersion        *int                `json:"schemaVersion,omitempty"`
	Trigger              WorkflowTriggerKind `json:"trigger"`
	TriggerConfiguration StringMap           `json:"triggerConfiguration,omitempty"`
	Nodes                []nodeRequest       `json:"nodes"`
	Edges                []edgeRequest       `json:"edges"`
}

type nodeRequest struct {
	ID            wire.UUID        `json:"id"`
	Kind          WorkflowNodeKind `json:"kind"`
	Name          string           `json:"name"`
	X             wire.Double      `json:"x"`
	Y             wire.Double      `json:"y"`
	Configuration StringMap        `json:"configuration"`
}

type edgeRequest struct {
	ID           wire.UUID `json:"id"`
	SourceNodeID wire.UUID `json:"sourceNodeID"`
	SourcePort   string    `json:"sourcePort"`
	TargetNodeID wire.UUID `json:"targetNodeID"`
	TargetPort   string    `json:"targetPort"`
}

// definition is the stored and returned form.
func (r definitionRequest) definition() WorkflowDefinitionDTO {
	version := 1
	if r.SchemaVersion != nil {
		version = *r.SchemaVersion
	}
	config := r.TriggerConfiguration
	if config == nil {
		config = StringMap{}
	}
	nodes := make([]WorkflowNodeDTO, len(r.Nodes))
	for i, n := range r.Nodes {
		nodes[i] = WorkflowNodeDTO{Id: n.ID, Kind: n.Kind, Name: n.Name, X: n.X, Y: n.Y, Configuration: n.Configuration}
	}
	edges := make([]WorkflowEdgeDTO, len(r.Edges))
	for i, e := range r.Edges {
		edges[i] = WorkflowEdgeDTO{Id: e.ID, SourceNodeID: e.SourceNodeID, SourcePort: e.SourcePort, TargetNodeID: e.TargetNodeID, TargetPort: e.TargetPort}
	}
	return WorkflowDefinitionDTO{SchemaVersion: &version, Trigger: r.Trigger, TriggerConfiguration: config, Nodes: nodes, Edges: edges}
}

type createRequest struct {
	Name            string            `json:"name"`
	DescriptionText *string           `json:"descriptionText,omitempty"`
	Definition      definitionRequest `json:"definition"`
}

type draftUpdateRequest struct {
	Name             *string           `json:"name,omitempty"`
	DescriptionText  *string           `json:"descriptionText,omitempty"`
	Enabled          *bool             `json:"enabled,omitempty"`
	ExpectedRevision int               `json:"expectedRevision"`
	Definition       definitionRequest `json:"definition"`
}

type runRequest struct {
	Input          StringMap  `json:"input"`
	ConversationID *wire.UUID `json:"conversationID,omitempty"`
}

type decisionRequest struct {
	Approved  bool        `json:"approved"`
	Note      *string     `json:"note,omitempty"`
	MemoryIDs []wire.UUID `json:"memoryIDs,omitempty"`
}

type instantiateRequest struct {
	Name *string `json:"name,omitempty"`
}

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

// UnmarshalJSON validates, in a request body and in a stored definition alike:
// Swift decodes the column with the same type, so a bad value there is a 500.
func (k *WorkflowTriggerKind) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "WorkflowTriggerKind", []string{string(Manual), string(Chat), string(Schedule), string(Webhook)})
	if err != nil {
		return err
	}
	*k = WorkflowTriggerKind(s)
	return nil
}

// UnmarshalJSON validates; see WorkflowTriggerKind.UnmarshalJSON.
func (k *WorkflowNodeKind) UnmarshalJSON(b []byte) error {
	s, err := unmarshalEnum(b, "WorkflowNodeKind", []string{
		string(Trigger), string(Llm), string(Skill), string(MemorySearch), string(MemoryWrite), string(Template),
		string(Condition), string(Approval), string(Parallel), string(ForEach), string(WhileLoop), string(Output),
	})
	if err != nil {
		return err
	}
	*k = WorkflowNodeKind(s)
	return nil
}

// decodeDefinition reads a draft_definition or definition column, with the same
// defaults as the request form.
func decodeDefinition(raw []byte) (WorkflowDefinitionDTO, error) {
	var def WorkflowDefinitionDTO
	if err := json.Unmarshal(raw, &def); err != nil {
		return def, err
	}
	if def.SchemaVersion == nil {
		version := 1
		def.SchemaVersion = &version
	}
	if def.TriggerConfiguration == nil {
		def.TriggerConfiguration = StringMap{}
	}
	if def.Nodes == nil {
		def.Nodes = []WorkflowNodeDTO{}
	}
	if def.Edges == nil {
		def.Edges = []WorkflowEdgeDTO{}
	}
	for i := range def.Nodes {
		if def.Nodes[i].Configuration == nil {
			def.Nodes[i].Configuration = StringMap{}
		}
	}
	return def, nil
}
