package llm

import "encoding/json"

// AssistantMessage is OpenAICompletionReader.assistantMessage: the first
// choice's message, or false if the body is not a well-formed completion.
func AssistantMessage(data []byte) (map[string]any, bool) {
	root, ok := decodeObject(data)
	if !ok {
		return nil, false
	}
	choices, ok := objects(root["choices"])
	if !ok || len(choices) == 0 {
		return nil, false
	}
	message, ok := choices[0]["message"].(map[string]any)
	return message, ok
}

// ToolCallNames is OpenAICompletionReader.toolCallNames(in:): the tools the
// model invoked, in call order. Non-nil and empty means it ran without tools.
func ToolCallNames(message map[string]any) []string {
	names := []string{}
	if calls, ok := objects(message["tool_calls"]); ok {
		for _, call := range calls {
			if function, ok := call["function"].(map[string]any); ok {
				if name, ok := function["name"].(string); ok && name != "" {
					names = append(names, name)
				}
			}
		}
		if len(names) > 0 {
			return names
		}
	}
	if function, ok := message["function_call"].(map[string]any); ok { // legacy single-function shape
		if name, ok := function["name"].(string); ok && name != "" {
			return []string{name}
		}
	}
	return names
}

// ToolCallNamesFrom is the same over a raw response body.
func ToolCallNamesFrom(data []byte) []string {
	message, ok := AssistantMessage(data)
	if !ok {
		return []string{}
	}
	return ToolCallNames(message)
}

// AgentToolCalls is the agent_turn_traces.tool_calls column. Count is
// authoritative, Names best-effort detail.
type AgentToolCalls struct {
	Names []string `json:"names"`
	Count int      `json:"count"`
}

// UnmarshalJSON is AgentToolCalls.init(from:). Rows written before count
// existed carry only names, so a missing count is the number of names, and
// missing names are an empty list.
func (c *AgentToolCalls) UnmarshalJSON(data []byte) error {
	var raw struct {
		Names []string `json:"names"`
		Count *int     `json:"count"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Names == nil {
		raw.Names = []string{}
	}
	c.Names = raw.Names
	c.Count = len(raw.Names)
	if raw.Count != nil {
		c.Count = *raw.Count
	}
	return nil
}

// ToolRecord is AgentTurnTraceRecorder.toolRecord. Nil only when nothing is
// known; an empty record means the turn ran without tools. A nil names slice is
// Swift's nil, an empty one its empty array.
func ToolRecord(names []string, count *int) *AgentToolCalls {
	if names != nil {
		return &AgentToolCalls{Names: names, Count: len(names)}
	}
	if count != nil {
		return &AgentToolCalls{Names: []string{}, Count: *count}
	}
	return nil
}
