package apple

import "strings"

// EmbedSource is PhotoIndexController.embedSource: trimmed OCR text, then
// "Scene: a, b" on its own line. ok is false when there is nothing to embed.
func EmbedSource(ocr *string, tags []string) (string, bool) {
	var parts []string
	if ocr != nil {
		if t := trimSpace(*ocr); t != "" {
			parts = append(parts, t)
		}
	}
	if joined := strings.Join(tags, ", "); joined != "" {
		parts = append(parts, "Scene: "+joined)
	}
	combined := strings.Join(parts, "\n")
	return combined, combined != ""
}
