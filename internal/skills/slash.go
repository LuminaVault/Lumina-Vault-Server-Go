package skills

import "lumina/internal/swiftstr"

// InvocationKind is a SlashCommandInvocation.Kind case.
type InvocationKind int

const (
	// KindKBCompile runs the memory compile.
	KindKBCompile InvocationKind = iota
	// KindSkill runs the skill in Invocation.SkillName.
	KindSkill
	// KindHelp answers with Invocation.HelpMarkdown.
	KindHelp
)

// Invocation is a parsed slash command.
type Invocation struct {
	Kind         InvocationKind
	SkillName    string
	HelpMarkdown string
	// Input is the text after the command, nil when there is none.
	Input     *string
	Arguments map[string]string
}

// ParseSlash is SlashCommandParser.parse. ok is false for anything but a slash
// command. An unknown command is a skill of that (lowercased) name.
func ParseSlash(raw string) (inv Invocation, ok bool) {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(raw)
	if !swiftstr.HasASCIIPrefix(trimmed, "/") {
		return Invocation{}, false
	}
	command, rest, ok := splitCommand(trimmed[1:])
	if !ok {
		return Invocation{}, false
	}
	var input *string
	if text := swiftstr.TrimWhitespacesAndNewlines(rest); text != "" {
		input = &text
	}
	command = swiftLowercased(command)
	switch command {
	case "kb-compile", "kb-ingest":
		return Invocation{Kind: KindKBCompile, Input: input, Arguments: map[string]string{}}, true
	case "patterns", "pattern-detector":
		return topicSkill("pattern-detector", input), true
	case "contradict", "contradiction-detector":
		return topicSkill("contradiction-detector", input), true
	case "beliefs", "belief-evolution":
		if input == nil {
			return Invocation{Kind: KindHelp, HelpMarkdown: "Usage: `/beliefs <topic>`", Arguments: map[string]string{}}, true
		}
		return topicSkill("belief-evolution", input), true
	}
	arguments := map[string]string{}
	if input != nil {
		arguments["input"] = *input
	}
	return Invocation{Kind: KindSkill, SkillName: command, Input: input, Arguments: arguments}, true
}

func topicSkill(name string, topic *string) Invocation {
	arguments := map[string]string{}
	if topic != nil {
		arguments["topic"] = *topic
	}
	return Invocation{Kind: KindSkill, SkillName: name, Input: topic, Arguments: arguments}
}

// splitCommand is s.split(maxSplits: 1, whereSeparator: \.isWhitespace): whitespace
// before the first word is skipped, and exactly one separator is consumed after it.
func splitCommand(s string) (command, rest string, ok bool) {
	start, i := 0, 0
	for i < len(s) {
		n := swiftstr.CharacterLen(s[i:])
		if !isWhitespaceCharacter(s[i : i+n]) {
			i += n
			continue
		}
		if start != i {
			return s[start:i], s[i+n:], true
		}
		i += n
		start = i
	}
	if start == len(s) {
		return "", "", false
	}
	return s[start:], "", true
}
