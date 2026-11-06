package workflows

import (
	"fmt"
	"strconv"

	"github.com/google/uuid"
)

const (
	maxIterations     = 20
	maxParallelNodes  = 8
	maxExpandedNodes  = 100
	currentSchema     = 1
	parallelGroupKey  = "parallelGroup"
	maxIterationsKey  = "maxIterations"
	defaultIterations = "1"
)

// validationError is WorkflowValidationError and WorkflowSchedulePolicyError. Its
// text is String(describing:) of the Swift case, which reaches clients inside
// "invalid_workflow:<reason>" and the validate route's issue code.
type validationError string

func (e validationError) Error() string { return string(e) }

const (
	errMissingTrigger    validationError = "missingTrigger"
	errDuplicateNodeID   validationError = "duplicateNodeID"
	errUnknownEdgeNode   validationError = "unknownEdgeNode"
	errCycle             validationError = "cycle"
	errUnreachableNode   validationError = "unreachableNode"
	errIterationBound    validationError = "invalidIterationBound"
	errParallelGroup     validationError = "parallelGroupTooLarge"
	errExpandedExecution validationError = "expandedExecutionTooLarge"
	errInvalidCron       validationError = "invalidCron"
)

func errUnsupportedSchema(version int) validationError {
	return validationError(fmt.Sprintf("unsupportedSchemaVersion(%d)", version))
}

// A nested enum prints fully qualified.
func errUnsupportedKind(kind WorkflowNodeKind) validationError {
	return validationError("unsupportedNodeKind(LuminaVaultShared.WorkflowNodeKind." + string(kind) + ")")
}

func errScheduleTooFrequent(minimumMinutes int) validationError {
	return validationError(fmt.Sprintf("scheduleTooFrequent(minimumMinutes: %d)", minimumMinutes))
}

// validateDefinition is WorkflowValidator.validate, with its checks in order.
func validateDefinition(def WorkflowDefinitionDTO) error {
	version := currentSchema
	if def.SchemaVersion != nil {
		version = *def.SchemaVersion
	}
	if version != currentSchema {
		return errUnsupportedSchema(version)
	}
	for _, n := range def.Nodes {
		if n.Kind == Parallel || n.Kind == ForEach || n.Kind == WhileLoop {
			return errUnsupportedKind(n.Kind)
		}
	}
	known := make(map[uuid.UUID]bool, len(def.Nodes))
	triggers := 0
	var trigger uuid.UUID
	for _, n := range def.Nodes {
		known[n.Id.UUID] = true
		if n.Kind == Trigger {
			if triggers == 0 {
				trigger = n.Id.UUID
			}
			triggers++
		}
	}
	if len(known) != len(def.Nodes) {
		return errDuplicateNodeID
	}
	if triggers != 1 {
		return errMissingTrigger
	}
	targets := make(map[uuid.UUID][]uuid.UUID, len(def.Edges))
	indegree := make(map[uuid.UUID]int, len(def.Nodes))
	for _, e := range def.Edges {
		if !known[e.SourceNodeID.UUID] || !known[e.TargetNodeID.UUID] {
			return errUnknownEdgeNode
		}
		targets[e.SourceNodeID.UUID] = append(targets[e.SourceNodeID.UUID], e.TargetNodeID.UUID)
		indegree[e.TargetNodeID.UUID]++
	}
	if err := validateBounds(def.Nodes); err != nil {
		return err
	}
	if countReachable(trigger, targets) != len(def.Nodes) {
		return errUnreachableNode
	}
	if countTopological(def.Nodes, targets, indegree) != len(def.Nodes) {
		return errCycle
	}
	return nil
}

// validateBounds checks each node's iteration bound, then the expanded total, then
// the parallel groups.
func validateBounds(nodes []WorkflowNodeDTO) error {
	expanded := 0
	groups := map[string]int{}
	iterations := make([]int, len(nodes))
	for i, n := range nodes {
		raw, ok := n.Configuration[maxIterationsKey]
		if !ok {
			raw = defaultIterations
		}
		// Int(String) ?? 0. Atoi takes what Int does: a sign, then ASCII digits.
		count, err := strconv.Atoi(raw)
		if err != nil || count < 1 || count > maxIterations {
			return errIterationBound
		}
		iterations[i] = count
	}
	for i, n := range nodes {
		if n.Kind == Trigger {
			expanded++
		} else {
			expanded += iterations[i]
		}
		if group, ok := n.Configuration[parallelGroupKey]; ok {
			groups[group]++
		}
	}
	if expanded > maxExpandedNodes {
		return errExpandedExecution
	}
	for _, count := range groups {
		if count > maxParallelNodes {
			return errParallelGroup
		}
	}
	return nil
}

func countReachable(start uuid.UUID, targets map[uuid.UUID][]uuid.UUID) int {
	reachable := map[uuid.UUID]bool{start: true}
	frontier := []uuid.UUID{start}
	for len(frontier) > 0 {
		next := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		for _, id := range targets[next] {
			if !reachable[id] {
				reachable[id] = true
				frontier = append(frontier, id)
			}
		}
	}
	return len(reachable)
}

// countTopological is Kahn's algorithm: fewer visits than nodes means a cycle.
func countTopological(nodes []WorkflowNodeDTO, targets map[uuid.UUID][]uuid.UUID, indegree map[uuid.UUID]int) int {
	remaining := make(map[uuid.UUID]int, len(nodes))
	var queue []uuid.UUID
	for _, n := range nodes {
		remaining[n.Id.UUID] = indegree[n.Id.UUID]
		if indegree[n.Id.UUID] == 0 {
			queue = append(queue, n.Id.UUID)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		visited++
		for _, target := range targets[id] {
			remaining[target]--
			if remaining[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	return visited
}
