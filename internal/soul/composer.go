package soul

import (
	"lumina/internal/swiftstr"
	"strconv"
	"strings"
	"time"
)

// ComposerVersion is SOULComposer.version, written into the front matter.
const ComposerVersion = 2

// SOULComposer's clamps.
const (
	maxAgentNameLength     = 64
	maxOtherPriorityLength = 256
	maxVoiceSamples        = 3
	maxVoiceSampleBytes    = 2 * 1024
)

// ComposeInput is LuminaVaultShared's SoulComposeRequest without dryRun. An empty
// value is the same as Swift's nil: every field falls back to its default. The
// enum fields hold the raw values, which callers check with the Valid* maps.
type ComposeInput struct {
	AgentName     string
	Tone          string
	Role          string
	Autonomy      string
	Priorities    []string
	OtherPriority string
	Format        string
	Length        string
	Emojis        bool
	VoiceSamples  []string
}

// Tones is SoulTone, raw value to the chat voice it renders.
var Tones = map[string]string{
	"warm":              "Warm, encouraging, and plain-spoken. Lead with empathy, stay direct.",
	"concise_technical": "A concise technical expert. No fluff — facts, code, and clear next steps.",
	"playful":           "Playful and light, with occasional wit. Never at the expense of clarity.",
	"coach":             "A direct coach. Challenge assumptions and push toward action.",
	"formal":            "Polished and professional. Complete sentences, measured wording, no slang.",
	"casual":            "Relaxed and conversational — like texting a sharp friend who gets things done.",
	"dry":               "Understated and matter-of-fact, with a dry wit. Never oversells.",
}

// Roles is SoulRole, raw value to the identity it renders.
var Roles = map[string]string{
	"assistant":    "your assistant — you ask, I do, and I keep things moving.",
	"coworker":     "your coworker — a peer who happens to know your whole context.",
	"coach":        "your coach — I track your goals and hold you to them.",
	"second_brain": "your second brain — I remember everything so you don't have to.",
}

// Autonomies is SoulAutonomy, raw value to the operations line it renders.
var Autonomies = map[string]string{
	"ask_first": "Confirm before any non-trivial action. When in doubt, ask.",
	"suggest":   "Propose actions and wait for a clear go-ahead before acting.",
	"act":       "Act on clear intent, then report what was done. Don't stall on confirmations.",
}

// Priorities is SoulPriority, raw value to its list item. "other" renders from the
// free-text field instead, so its line is empty.
var Priorities = map[string]string{
	"focus":    "- Deep focus and getting the important things done.",
	"health":   "- Health, energy, and sustainable routines.",
	"learning": "- Learning and leveling up skills.",
	"family":   "- Family and the people who matter.",
	"money":    "- Money, financial clarity, and long-term security.",
	"creative": "- Creative work and ideas.",
	"other":    "",
}

// Formats is SoulFormat, raw value to its line.
var Formats = map[string]string{
	"bullets": "Bullets and short sections over walls of text.",
	"prose":   "Flowing prose over bullet fragments.",
}

// Lengths is SoulLength, raw value to its line.
var Lengths = map[string]string{
	"short": "Short and scannable by default; go deep only when asked.",
	"long":  "Thorough by default — depth is welcome when it earns its length.",
}

func pick(table map[string]string, value, fallback string) string {
	if value == "" {
		value = fallback
	}
	return table[value]
}

// Compose is SOULComposer.render: the onboarding answers rendered into a SOUL.md,
// with the core covenant already where Inject puts it. It's deterministic but for
// created_at.
func Compose(in ComposeInput, username string, now time.Time) string {
	name := swiftstr.CharacterPrefix(swiftstr.TrimWhitespacesAndNewlines(in.AgentName), maxAgentNameLength)
	if name == "" {
		name = "Hermes"
	}
	emojiLine := "No emojis."
	if in.Emojis {
		emojiLine = "Emojis are welcome where they add signal."
	}
	return "---\nversion: " + strconv.Itoa(ComposerVersion) + "\nusername: " + username +
		"\ncreated_at: " + now.UTC().Format("2006-01-02T15:04:05Z") + "\n---\n\n" +
		"# SOUL.md\n\n" + Core + "\n\n" +
		"## Identity\n\nI am " + name + ", " + pick(Roles, in.Role, "second_brain") +
		" I read this file on every reply, so it defines who I am for " + username + ".\n\n" +
		"## Chat voice\n\n" + pick(Tones, in.Tone, "warm") + "\n\n" +
		"- Format: " + pick(Formats, in.Format, "bullets") + "\n" +
		"- Length: " + pick(Lengths, in.Length, "short") + "\n" +
		"- " + emojiLine + "\n\n" +
		"## Published content voice\n\nWhen drafting anything that leaves this vault (posts, replies, messages on connected channels), switch to a neutral, professional voice: clear, factual, no inside jokes, no emojis unless the platform calls for them. Publishing itself always requires explicit authorization (see core covenant).\n\n" +
		prioritiesSection(in) + "\n\n" +
		"## Operations\n\n" + pick(Autonomies, in.Autonomy, "suggest") +
		" Keep continuity across sessions by leaning on memory rather than re-asking.\n" +
		voiceSamplesSection(in) +
		"\n<!-- Anything below this line is free-form notes the user wrote about themselves. -->"
}

func prioritiesSection(in ComposeInput) string {
	var lines []string
	for _, p := range in.Priorities {
		if line := Priorities[p]; line != "" {
			lines = append(lines, line)
		}
	}
	if other := swiftstr.TrimWhitespacesAndNewlines(in.OtherPriority); other != "" {
		lines = append(lines, "- "+sanitizeInline(swiftstr.CharacterPrefix(other, maxOtherPriorityLength)))
	}
	if len(lines) == 0 {
		lines = append(lines, "- Whatever the current mission needs — tell me and update this list.")
	}
	return "## What matters to me\n\n" + strings.Join(lines, "\n")
}

func voiceSamplesSection(in ComposeInput) string {
	var quoted []string
	for _, sample := range in.VoiceSamples {
		sample = swiftstr.TrimWhitespacesAndNewlines(sample)
		if sample == "" {
			continue
		}
		if len(quoted) == maxVoiceSamples {
			break
		}
		quoted = append(quoted, quote(truncateUTF8(sample, maxVoiceSampleBytes)))
	}
	if len(quoted) == 0 {
		return ""
	}
	return "\n## How I talk\n\nMirror these voice samples when replying to me:\n\n" + strings.Join(quoted, "\n>\n") + "\n"
}

// quote prefixes each line with "> ". Swift splits on the Character "\n", so a
// "\r\n" (one Character) doesn't split.
func quote(sample string) string {
	var b strings.Builder
	b.WriteString("> ")
	for _, c := range swiftstr.Characters(sample) {
		if c == "\n" {
			b.WriteString("\n> ")
			continue
		}
		b.WriteString(c)
	}
	return b.String()
}

// sanitizeInline keeps free text inside its list item and out of HTML comments,
// one replacement after another as Swift chains them. Foundation's
// replacingOccurrences matches UTF-16 code units, so the "\n" of a "\r\n" is
// replaced too (a Darwin vector confirms it).
func sanitizeInline(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "<!--", "")
	return strings.ReplaceAll(s, "-->", "")
}

// truncateUTF8 is SOULComposer.truncateUTF8: comment delimiters go, then whole
// Characters come off the end, a quarter of the excess at a time, until the UTF-8
// size fits.
func truncateUTF8(s string, maxBytes int) string {
	out := strings.ReplaceAll(strings.ReplaceAll(s, "<!--", ""), "-->", "")
	for len(out) > maxBytes {
		chars := swiftstr.Characters(out)
		drop := max(1, (len(out)-maxBytes)/4)
		if drop >= len(chars) {
			return ""
		}
		out = strings.Join(chars[:len(chars)-drop], "")
	}
	return out
}
