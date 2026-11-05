// Package skills is the catalog half of Swift's Sources/App/Skills: the SKILL.md
// parser (SkillManifest.swift), the merged built-in and vault catalog
// (SkillCatalog.swift), the slash command parser (SlashCommandParser.swift) and
// the context router's selector and body rewrite (ContextRouterSelector.swift,
// ContextRouter.swift). The runner, the scheduler and the routes are not here.
package skills

import (
	"errors"
	"fmt"
	"io"
	"lumina/internal/swiftstr"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Source says where a manifest came from.
type Source string

const (
	SourceBuiltin Source = "builtin"
	SourceVault   Source = "vault"
)

// Capability is metadata.capability.
type Capability string

const (
	CapabilityLow    Capability = "low"
	CapabilityMedium Capability = "medium"
	CapabilityHigh   Capability = "high"
)

// OutputKind is metadata.outputs[].kind.
type OutputKind string

const (
	OutputMemo         OutputKind = "memo"
	OutputAPNSDigest   OutputKind = "apns_digest"
	OutputAPNSNudge    OutputKind = "apns_nudge"
	OutputMemoryEmit   OutputKind = "memory_emit"
	OutputVaultRewrite OutputKind = "vault_rewrite"
	OutputChatMessage  OutputKind = "chat_message"
)

// Output is one metadata.outputs entry.
type Output struct {
	Kind     OutputKind
	Path     *string
	Category *string
}

// DailyRunCap is metadata.daily_run_cap. 0 means unlimited for that tier.
type DailyRunCap struct {
	Trial    int
	Pro      int
	Ultimate int
}

// Value is the cap for a users.tier string. An unknown tier gets the trial cap.
func (c DailyRunCap) Value(tier string) int {
	switch swiftLowercased(tier) {
	case "ultimate":
		return c.Ultimate
	case "pro":
		return c.Pro
	default:
		return c.Trial
	}
}

// Manifest is a parsed SKILL.md. Treat its slices as read-only: a catalog hands
// the same built-in manifests to every caller.
type Manifest struct {
	Source       Source
	Name         string
	Description  string
	AllowedTools []string
	Capability   Capability
	// Schedule is nil for a missing or blank schedule, which means manual only.
	Schedule    *string
	OnEvent     []string
	Outputs     []Output
	DailyRunCap *DailyRunCap
	Body        string
}

// ManifestErrorKind is a SkillManifestError case.
type ManifestErrorKind int

const (
	MissingFrontmatter ManifestErrorKind = iota
	MalformedFrontmatter
	MissingRequiredField
	InvalidCapability
	InvalidOutputKind
)

// ManifestError is why a SKILL.md was rejected. A manifest never partly loads.
// Detail is the field or value at fault; for MalformedFrontmatter it is a
// description for the log and doesn't match Swift's text.
type ManifestError struct {
	Kind   ManifestErrorKind
	Detail string
}

func (e *ManifestError) Error() string {
	switch e.Kind {
	case MissingFrontmatter:
		return "missingFrontmatter"
	case MalformedFrontmatter:
		return "malformedFrontmatter: " + e.Detail
	case MissingRequiredField:
		return "missingRequiredField: " + e.Detail
	case InvalidCapability:
		return "invalidCapability: " + e.Detail
	default:
		return "invalidOutputKind: " + e.Detail
	}
}

func malformed(format string, args ...any) error {
	return &ManifestError{Kind: MalformedFrontmatter, Detail: fmt.Sprintf(format, args...)}
}

// Parse is SkillManifestParser.parse. contents is what String(contentsOf:) gives,
// so a byte order mark has already gone; the catalog removes it before calling.
func Parse(source Source, contents string) (Manifest, error) {
	frontmatter, body, err := splitFrontmatter(contents)
	if err != nil {
		return Manifest{}, err
	}
	raw, err := decodeFrontmatter(frontmatter)
	if err != nil {
		return Manifest{}, err
	}
	return build(source, raw, body)
}

// splitFrontmatter takes the first line equal to "---", give or take spaces and
// tabs, as the opening, and the next such line as the close. CRLF files have no
// such first line (swiftstr.SplitLines), so they are MissingFrontmatter, as in
// Swift on Linux.
func splitFrontmatter(contents string) (frontmatter, body string, err error) {
	lines := swiftstr.SplitLines(contents)
	if swiftstr.TrimWhitespaces(lines[0]) != "---" {
		return "", "", &ManifestError{Kind: MissingFrontmatter}
	}
	for i := 1; i < len(lines); i++ {
		if swiftstr.TrimWhitespaces(lines[i]) == "---" {
			frontmatter = strings.Join(lines[1:i], "\n")
			body = strings.Join(lines[i+1:], "\n")
			return frontmatter, swiftstr.TrimWhitespacesAndNewlines(body), nil
		}
	}
	return "", "", malformed("frontmatter has no closing `---` delimiter")
}

func build(source Source, raw rawManifest, body string) (Manifest, error) {
	name := swiftstr.TrimWhitespacesAndNewlines(deref(raw.name))
	if name == "" {
		return Manifest{}, &ManifestError{Kind: MissingRequiredField, Detail: "name"}
	}
	description := swiftstr.TrimWhitespacesAndNewlines(deref(raw.description))
	if description == "" {
		return Manifest{}, &ManifestError{Kind: MissingRequiredField, Detail: "description"}
	}
	if raw.capability == nil {
		return Manifest{}, &ManifestError{Kind: MissingRequiredField, Detail: "metadata.capability"}
	}
	capability := Capability(*raw.capability)
	switch capability {
	case CapabilityLow, CapabilityMedium, CapabilityHigh:
	default:
		return Manifest{}, &ManifestError{Kind: InvalidCapability, Detail: *raw.capability}
	}
	outputs := make([]Output, 0, len(raw.outputs))
	for _, o := range raw.outputs {
		kind := OutputKind(o.kind)
		switch kind {
		case OutputMemo, OutputAPNSDigest, OutputAPNSNudge, OutputMemoryEmit, OutputVaultRewrite, OutputChatMessage:
		default:
			return Manifest{}, &ManifestError{Kind: InvalidOutputKind, Detail: o.kind}
		}
		outputs = append(outputs, Output{Kind: kind, Path: o.path, Category: o.category})
	}
	schedule := raw.schedule
	if schedule != nil && swiftstr.TrimWhitespaces(*schedule) == "" {
		schedule = nil
	}
	return Manifest{
		Source:       source,
		Name:         name,
		Description:  description,
		AllowedTools: raw.allowedTools,
		Capability:   capability,
		Schedule:     schedule,
		OnEvent:      raw.onEvent,
		Outputs:      outputs,
		DailyRunCap:  raw.dailyRunCap,
		Body:         body,
	}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// MARK: - Frontmatter decode

// rawManifest is what RawManifest decodes to.
type rawManifest struct {
	name, description *string
	allowedTools      []string
	capability        *string
	schedule          *string
	onEvent           []string
	outputs           []rawOutput
	dailyRunCap       *DailyRunCap
}

type rawOutput struct {
	kind           string
	path, category *string
}

// decodeFrontmatter is YAMLDecoder().decode(RawManifest.self, from:) from Yams
// 5.4.0, whose rules differ from yaml.v3's decoding: it resolves no implicit types
// (every scalar is its raw text when asked for a String), rejects duplicate keys
// and self-referencing aliases, and has its own integer parser. So this walks
// yaml.v3's node tree by hand, fields in Swift's declaration order, because the
// first failure decides the error. testdata/manifest_oracle.json holds what the
// real parser said about each case.
func decodeFrontmatter(frontmatter string) (rawManifest, error) {
	dec := yaml.NewDecoder(strings.NewReader(frontmatter))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return rawManifest{}, malformed("frontmatter is empty")
		}
		return rawManifest{}, malformed("not valid YAML: %v", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return rawManifest{}, malformed("not a single YAML document")
	}
	root := resolve(&doc)
	if err := validateTree(root, map[*yaml.Node]bool{}); err != nil {
		return rawManifest{}, err
	}
	budget := maxFlattenedPairs
	top, err := keyed(root, &budget)
	if err != nil {
		return rawManifest{}, err
	}
	var raw rawManifest
	if raw.name, err = optionalString(top, "name"); err != nil {
		return raw, err
	}
	if raw.description, err = optionalString(top, "description"); err != nil {
		return raw, err
	}
	if raw.allowedTools, err = stringOrArray(top, "allowed-tools"); err != nil {
		return raw, err
	}
	raw.onEvent = []string{}
	node := lookup(top, "metadata")
	if node == nil || isNull(node) {
		return raw, nil
	}
	meta, err := keyed(node, &budget)
	if err != nil {
		return raw, err
	}
	if raw.capability, err = optionalString(meta, "capability"); err != nil {
		return raw, err
	}
	if raw.schedule, err = optionalString(meta, "schedule"); err != nil {
		return raw, err
	}
	if events, err := optionalStrings(meta, "on_event"); err != nil {
		return raw, err
	} else if events != nil {
		raw.onEvent = events
	}
	if raw.outputs, err = decodeOutputs(meta, &budget); err != nil {
		return raw, err
	}
	raw.dailyRunCap, err = decodeDailyRunCap(meta, &budget)
	return raw, err
}

func decodeOutputs(meta []pair, budget *int) ([]rawOutput, error) {
	node := lookup(meta, "outputs")
	if node == nil || isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, malformed("outputs is not a sequence")
	}
	outputs := make([]rawOutput, 0, len(node.Content))
	for _, item := range node.Content {
		fields, err := keyed(resolve(item), budget)
		if err != nil {
			return nil, err
		}
		kind := lookup(fields, "kind")
		if kind == nil {
			return nil, &ManifestError{Kind: MissingRequiredField, Detail: "kind"}
		}
		if kind.Kind != yaml.ScalarNode {
			return nil, malformed("kind is not a scalar")
		}
		out := rawOutput{kind: kind.Value}
		if out.path, err = optionalString(fields, "path"); err != nil {
			return nil, err
		}
		if out.category, err = optionalString(fields, "category"); err != nil {
			return nil, err
		}
		outputs = append(outputs, out)
	}
	return outputs, nil
}

func decodeDailyRunCap(meta []pair, budget *int) (*DailyRunCap, error) {
	node := lookup(meta, "daily_run_cap")
	if node == nil || isNull(node) {
		return nil, nil
	}
	fields, err := keyed(node, budget)
	if err != nil {
		return nil, err
	}
	var limit DailyRunCap
	for _, f := range []struct {
		key string
		to  *int
	}{{"trial", &limit.Trial}, {"pro", &limit.Pro}, {"ultimate", &limit.Ultimate}} {
		value := lookup(fields, f.key)
		if value == nil {
			return nil, &ManifestError{Kind: MissingRequiredField, Detail: f.key}
		}
		n, ok := yamsInt(value)
		if !ok {
			return nil, malformed("%s is not an integer", f.key)
		}
		*f.to = n
	}
	return &limit, nil
}

// optionalString is decodeIfPresent(String.self): absent or null is nil, any other
// scalar is its text, and a collection fails.
func optionalString(fields []pair, key string) (*string, error) {
	node := lookup(fields, key)
	if node == nil || isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.ScalarNode {
		return nil, malformed("%s is not a scalar", key)
	}
	value := node.Value
	return &value, nil
}

// optionalStrings is decodeIfPresent([String].self). An element isn't checked for
// null, so `[a, ~]` holds the two strings "a" and "~".
func optionalStrings(fields []pair, key string) ([]string, error) {
	node := lookup(fields, key)
	if node == nil || isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, malformed("%s is not a sequence", key)
	}
	return sequenceStrings(node, key)
}

func sequenceStrings(node *yaml.Node, key string) ([]string, error) {
	values := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		item = resolve(item)
		if item.Kind != yaml.ScalarNode {
			return nil, malformed("%s holds something that is not a scalar", key)
		}
		values = append(values, item.Value)
	}
	return values, nil
}

// stringOrArray is StringOrArray.toArray() with `?? []`: a scalar splits on
// whitespace Characters, a sequence is taken as it is.
func stringOrArray(fields []pair, key string) ([]string, error) {
	node := lookup(fields, key)
	if node == nil || isNull(node) {
		return []string{}, nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return splitWhitespace(node.Value), nil
	case yaml.SequenceNode:
		return sequenceStrings(node, key)
	default:
		return nil, malformed("%s is neither a string nor an array", key)
	}
}

// MARK: - Node helpers

type pair struct{ key, value *yaml.Node }

// maxFlattenedPairs stops a merge-key bomb. Swift has no such limit and would run
// out of memory on a manifest that hits this; here it's just malformed.
const maxFlattenedPairs = 1 << 16

// resolve unwraps document and alias nodes. Yams has already done this by the time
// it decodes.
func resolve(n *yaml.Node) *yaml.Node {
	for {
		switch n.Kind {
		case yaml.DocumentNode:
			n = n.Content[0]
		case yaml.AliasNode:
			n = n.Alias
		default:
			return n
		}
	}
}

// validateTree applies the two checks Yams makes while composing, anywhere in the
// document, even under keys nobody reads: duplicate keys, and an alias to a node
// still being composed, which it reports as undefined.
func validateTree(n *yaml.Node, open map[*yaml.Node]bool) error {
	if n.Kind == yaml.AliasNode {
		if open[n.Alias] {
			return malformed("alias %q refers to an enclosing node", n.Value)
		}
		return nil
	}
	open[n] = true
	defer delete(open, n)
	if n.Kind == yaml.MappingNode {
		seen := make(map[string]bool, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			id := nodeIdentity(n.Content[i], 0)
			if seen[id] {
				return malformed("duplicate key %q", resolve(n.Content[i]).Value)
			}
			seen[id] = true
		}
	}
	for _, child := range n.Content {
		if err := validateTree(child, open); err != nil {
			return err
		}
	}
	return nil
}

// nodeIdentity is Node's Hashable identity: text and resolved tag, with String's
// canonical equivalence. Every scalar without an explicit tag is a string here,
// because Yams decodes with only the merge resolver, unlike yaml.v3's Tag.
func nodeIdentity(n *yaml.Node, depth int) string {
	n = resolve(n)
	if depth > 64 {
		return "deep"
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return "s:" + keyTag(n) + ":" + swiftstr.CanonicalKey(n.Value)
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "c%d:%s[", n.Kind, n.ShortTag())
		for _, child := range n.Content {
			b.WriteString(strconv.Quote(nodeIdentity(child, depth+1)))
			b.WriteByte(',')
		}
		b.WriteByte(']')
		return b.String()
	}
}

func keyTag(n *yaml.Node) string {
	if n.Style&yaml.TaggedStyle != 0 {
		return n.ShortTag()
	}
	if n.Tag == "!!merge" {
		return "!!merge"
	}
	return "!!str"
}

// isNull is Yams' NSNull.construct: plain style only, so `”` isn't null, and an
// explicit tag doesn't matter.
func isNull(n *yaml.Node) bool {
	if n.Kind != yaml.ScalarNode || !isPlain(n) {
		return false
	}
	switch n.Value {
	case "", "~", "null", "Null", "NULL":
		return true
	}
	return false
}

func isPlain(n *yaml.Node) bool {
	return n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0
}

// keyed is container(keyedBy:): the node's pairs after Node.Mapping.flatten().
func keyed(n *yaml.Node, budget *int) ([]pair, error) {
	if n.Kind != yaml.MappingNode {
		return nil, malformed("expected a mapping")
	}
	return flatten(n, budget)
}

// flatten moves merged pairs in front of the mapping's own. lookup takes the last
// match, so own keys win, and of several merged mappings the first listed wins. A
// merge value that is neither a mapping nor a sequence is ignored.
func flatten(n *yaml.Node, budget *int) ([]pair, error) {
	var merged, own []pair
	for i := 0; i+1 < len(n.Content); i += 2 {
		*budget--
		if *budget < 0 {
			return nil, malformed("too many merged keys")
		}
		key, value := resolve(n.Content[i]), resolve(n.Content[i+1])
		if key.Kind != yaml.ScalarNode || keyTag(key) != "!!merge" {
			own = append(own, pair{key, value})
			continue
		}
		switch value.Kind {
		case yaml.MappingNode:
			pairs, err := flatten(value, budget)
			if err != nil {
				return nil, err
			}
			merged = append(merged, pairs...)
		case yaml.SequenceNode:
			for j := len(value.Content) - 1; j >= 0; j-- {
				item := resolve(value.Content[j])
				if item.Kind != yaml.MappingNode {
					continue
				}
				pairs, err := flatten(item, budget)
				if err != nil {
					return nil, err
				}
				merged = append(merged, pairs...)
			}
		}
	}
	return append(merged, own...), nil
}

// lookup is Node.Mapping's subscript: the last pair whose key is that string. A key
// with some other explicit tag is a different key.
func lookup(fields []pair, key string) *yaml.Node {
	for i := len(fields) - 1; i >= 0; i-- {
		k := fields[i].key
		if k.Kind == yaml.ScalarNode && keyTag(k) == "!!str" && k.Value == key {
			return fields[i].value
		}
	}
	return nil
}

// yamsInt is Int.construct(from: Node.Scalar): plain style, underscores dropped,
// 0x/0b/0o/0 radix prefixes (a leading 0 is always octal, so 09 fails), sexagesimal,
// then decimal.
func yamsInt(n *yaml.Node) (int, bool) {
	if n.Kind != yaml.ScalarNode || !isPlain(n) {
		return 0, false
	}
	text := strings.ReplaceAll(n.Value, "_", "")
	if text == "0" {
		return 0, true
	}
	negative := strings.HasPrefix(text, "-")
	digits, sign := text, ""
	if negative {
		digits, sign = text[1:], "-"
	} else if strings.HasPrefix(text, "+") {
		digits = text[1:]
	}
	for _, p := range []struct {
		prefix string
		radix  int
	}{{"0x", 16}, {"0b", 2}, {"0o", 8}, {"0", 8}} {
		if strings.HasPrefix(digits, p.prefix) {
			return swiftInt(sign+digits[len(p.prefix):], p.radix)
		}
	}
	if strings.Contains(digits, ":") {
		return sexagesimal(digits, negative)
	}
	return swiftInt(text, 10)
}

// swiftInt is Int(_:radix:): an optional sign, then at least one digit.
func swiftInt(text string, radix int) (int, bool) {
	n, err := strconv.ParseInt(text, radix, 64)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// sexagesimal is String.sexagesimal(), base 60 with each part read by Int(_:). Swift
// traps on overflow (the base alone overflows from eleven parts); here it fails.
func sexagesimal(digits string, negative bool) (int, bool) {
	parts := strings.Split(digits, ":")
	if len(parts) > 10 {
		return 0, false
	}
	total, base := new(big.Int), big.NewInt(1)
	sixty := big.NewInt(60)
	for i := len(parts) - 1; i >= 0; i-- {
		part, ok := swiftInt(parts[i], 10)
		if !ok {
			return 0, false
		}
		total.Add(total, new(big.Int).Mul(big.NewInt(int64(part)), base))
		base.Mul(base, sixty)
	}
	if negative {
		total.Neg(total)
	}
	if !total.IsInt64() {
		return 0, false
	}
	return int(total.Int64()), true
}

// MARK: - Swift string helpers

// isWhitespaceCharacter is Character.isWhitespace, which looks at the first scalar.
func isWhitespaceCharacter(c string) bool {
	r, _ := utf8.DecodeRuneInString(c)
	return unicode.Is(unicode.White_Space, r)
}

// splitWhitespace is s.split(whereSeparator: \.isWhitespace).
func splitWhitespace(s string) []string {
	parts := []string{}
	start, i := 0, 0
	for i < len(s) {
		n := swiftstr.CharacterLen(s[i:])
		if isWhitespaceCharacter(s[i : i+n]) {
			if start != i {
				parts = append(parts, s[start:i])
			}
			start = i + n
		}
		i += n
	}
	if start != len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

// swiftLowercased is String.lowercased(): each scalar's own mapping with no context,
// so unlike golang.org/x/text/cases there is no final sigma, and unlike
// strings.ToLower U+0130 becomes two scalars.
func swiftLowercased(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 0x130 {
			b.WriteString("i̇")
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
