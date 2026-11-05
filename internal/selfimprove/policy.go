package selfimprove

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"unicode"
)

// maxResourceNameCharacters is validResourceName's limit, in Swift Characters.
const maxResourceNameCharacters = 80

// ValidResourceName is SelfImprovementService.validResourceName: one to 80
// Characters, each a letter, a number, "-" or "_". Character.isLetter and isNumber
// read the first scalar only, so a letter with a combining mark passes.
func ValidResourceName(name string) bool {
	characters := swiftstr.Characters(name)
	if len(characters) == 0 || len(characters) > maxResourceNameCharacters {
		return false
	}
	for _, c := range characters {
		if !isLetterOrNumber(c) && !swiftstr.CharacterIs(c, '-') && !swiftstr.CharacterIs(c, '_') {
			return false
		}
	}
	return true
}

// isLetterOrNumber is Character.isLetter (Alphabetic) or Character.isNumber (any
// numeric type) on the first scalar. Ideographs with a numeric value are letters
// already, so the N categories cover the rest of numericType.
func isLetterOrNumber(character string) bool {
	r := []rune(character)[0]
	return unicode.IsLetter(r) || unicode.Is(unicode.N, r) || unicode.Is(unicode.Other_Alphabetic, r)
}

// SHA256 is SelfImprovementService.sha256: lowercase hex of the UTF-8 bytes.
func SHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// IsConservativeChange is the line-set gate on SOUL proposals: at most
// max(20, a third of the old distinct lines) lines may differ, in either direction.
// The review that calls it is in SelfImprovementScheduler, which stays in Swift
// until the LLM direct path ports; it's here with its tests for that port.
func IsConservativeChange(from, to string) bool {
	oldLines, newLines := lineSet(from), lineSet(to)
	changed := 0
	for line := range oldLines {
		if !newLines[line] {
			changed++
		}
	}
	for line := range newLines {
		if !oldLines[line] {
			changed++
		}
	}
	return changed <= max(20, len(oldLines)/3)
}

// lineSet is Set(s.split(separator: "\n")): empty lines don't count, and Swift's
// String equality is canonical equivalence.
func lineSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, line := range swiftstr.SplitLines(s) {
		if line != "" {
			set[swiftstr.CanonicalKey(line)] = true
		}
	}
	return set
}

// DecodeJSON is SelfImprovementService.decodeJSON: the model's reply as JSON, with
// surrounding whitespace and a Markdown fence removed. Like IsConservativeChange,
// it has no caller until the scheduler ports.
func DecodeJSON[T any](raw string) (T, error) {
	var out T
	unfenced := swiftstr.TrimWhitespacesAndNewlines(raw)
	if swiftstr.HasASCIIPrefix(unfenced, "```") {
		lines := swiftstr.SplitLines(unfenced)
		if len(lines) < 2 {
			lines = nil
		} else {
			lines = lines[1 : len(lines)-1]
		}
		unfenced = strings.Join(lines, "\n")
	}
	if err := json.Unmarshal([]byte(unfenced), &out); err != nil {
		return out, wire.NewError(http.StatusBadGateway, "review_output_invalid_json")
	}
	return out, nil
}
