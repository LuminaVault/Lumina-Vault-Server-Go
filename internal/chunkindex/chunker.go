// Package chunkindex is the retrieval unit that carries a citation: the markdown
// chunker, the wikilink parser and resolver, the deterministic chunk IDs, the
// repositories over memory_chunks and vault_links, the chunk indexer, and the
// hybrid search that merges the chunk arm with the document arm in memorystore.
//
// It ports Sources/App/Indexing and Repositories/MemoryChunkRepository.swift. Both
// servers write the same rows while they run side by side, so everything that
// reaches a hash reproduces Swift's String semantics exactly, on the platform
// production runs: Characters are extended grapheme clusters, not bytes or runes,
// and a "\r\n" does not split a line. testdata/swift_golden.json holds output from
// Swift's own sources (tools/swiftgolden) and golden_test.go checks it.
//
// Inputs must be valid UTF-8: a Swift String cannot hold anything else, and
// Postgres refuses it.
package chunkindex

import (
	"lumina/internal/swiftstr"
	"strings"
)

// Chunker defaults: about 600 tokens, inside the embedding window.
const (
	DefaultMaxChars     = 2400
	DefaultOverlapChars = 200
)

// DocumentChunk is a source-preserving slice of a document. Text is exactly the
// source lines StartLine...EndLine (1-based, inclusive) joined by "\n".
type DocumentChunk struct {
	// Ordinal is the 1-based position, contiguous across the chunk list.
	Ordinal int
	// HeadingPath is the section's heading ancestry, outermost first. Never nil.
	HeadingPath   []string
	StartLine     int
	EndLine       int
	Text          string
	ContentSHA256 string
}

// Chunk is MarkdownChunker.chunk with the default sizes.
func Chunk(source string) []DocumentChunk {
	return ChunkSized(source, DefaultMaxChars, DefaultOverlapChars)
}

// ChunkSized is MarkdownChunker.chunk. Sections are cut at headings; a section
// over maxChars splits at paragraph boundaries and a paragraph over maxChars at
// line boundaries, carrying overlapChars of trailing context forward. A line over
// maxChars is emitted whole. Sizes are in Characters.
func ChunkSized(source string, maxChars, overlapChars int) []DocumentChunk {
	maxChars = max(1, maxChars)
	overlapChars = max(0, min(overlapChars, maxChars-1))

	d := newDocument(source)
	bodyStart := d.bodyStartLine()
	if bodyStart > len(d.lines) {
		return []DocumentChunk{}
	}

	// Headings come from the body only: a frontmatter block's closing "---" right
	// after a "key: value" line would otherwise read as a setext h2.
	bodyLines := d.lines[bodyStart-1:]
	offset := bodyStart - 1
	headings := ExtractHeadings(bodyLines)
	for i := range headings {
		headings[i].Line += offset
	}
	hierarchy := HeadingHierarchy(headings)
	ordinalByLine := make(map[int]int, len(headings))
	for _, h := range headings {
		if _, taken := ordinalByLine[h.Line]; !taken {
			ordinalByLine[h.Line] = h.Ordinal
		}
	}

	chunks := []DocumentChunk{}
	for _, section := range d.sections(bodyStart, ordinalByLine, hierarchy) {
		chunks = append(chunks, d.split(section, maxChars, overlapChars)...)
	}
	for i := range chunks {
		chunks[i].Ordinal = i + 1
	}
	return chunks
}

type section struct {
	start, end  int
	headingPath []string
}

// document holds the source lines and each line's Character count. Swift counts
// on every measure; the prefix sum gives the same numbers without the quadratic
// walk.
type document struct {
	lines []string
	sum   []int
}

func newDocument(source string) *document {
	lines := swiftstr.SplitLines(source)
	sum := make([]int, len(lines)+1)
	for i, line := range lines {
		sum[i+1] = sum[i] + swiftstr.CharacterCount(line)
	}
	return &document{lines: lines, sum: sum}
}

func (d *document) line(n int) string { return d.lines[n-1] }

func (d *document) blank(n int) bool { return swiftstr.TrimWhitespaces(d.line(n)) == "" }

// bodyStartLine is the 1-based line where the body begins, past a leading YAML
// frontmatter block. Unterminated frontmatter counts as body rather than silently
// dropping every line.
func (d *document) bodyStartLine() int {
	if swiftstr.TrimWhitespaces(d.lines[0]) != "---" {
		return 1
	}
	for index := 1; index < len(d.lines); index++ {
		if swiftstr.TrimWhitespaces(d.lines[index]) == "---" {
			return index + 2
		}
	}
	return 1
}

// sections cuts the body: one per heading plus any preamble before the first.
func (d *document) sections(bodyStart int, ordinalByLine map[int]int, hierarchy map[int][]string) []section {
	var result []section
	sectionStart := bodyStart
	current := 0 // no heading yet; ordinals start at 1
	path := func() []string {
		if p, ok := hierarchy[current]; ok {
			return p
		}
		return []string{}
	}
	for line := bodyStart; line <= len(d.lines); line++ {
		ordinal, isHeading := ordinalByLine[line]
		if !isHeading {
			continue
		}
		if sectionStart < line {
			result = append(result, section{sectionStart, line - 1, path()})
		}
		sectionStart = line
		current = ordinal
	}
	if sectionStart <= len(d.lines) {
		result = append(result, section{sectionStart, len(d.lines), path()})
	}
	return result
}

// split cuts one section into chunks that each fit maxChars.
func (d *document) split(s section, maxChars, overlapChars int) []DocumentChunk {
	first, last, ok := d.trimBlankEdges(s.start, s.end)
	if !ok {
		return nil
	}
	if d.sliceLength(first, last) <= maxChars {
		return []DocumentChunk{d.makeChunk(first, last, s.headingPath)}
	}

	var chunks []DocumentChunk
	pendingStart, pendingEnd, pending := 0, 0, false

	for _, para := range d.paragraphs(first, last) {
		if pending {
			if d.sliceLength(pendingStart, para.end) <= maxChars {
				pendingEnd = para.end
				continue
			}
			chunks = append(chunks, d.makeChunk(pendingStart, pendingEnd, s.headingPath))
			pending = false
		}

		// A paragraph that cannot fit on its own splits at line boundaries.
		if d.sliceLength(para.start, para.end) > maxChars {
			chunks = append(chunks, d.splitByLines(para.start, para.end, s.headingPath, maxChars, overlapChars)...)
			continue
		}

		notBefore := first
		if len(chunks) > 0 {
			notBefore = chunks[len(chunks)-1].StartLine + 1
		}
		pendingStart = d.overlapStart(para.start, notBefore, overlapChars)
		pendingEnd = para.end
		pending = true
	}
	if pending {
		chunks = append(chunks, d.makeChunk(pendingStart, pendingEnd, s.headingPath))
	}
	return chunks
}

// splitByLines cuts an oversized paragraph into line-aligned windows.
func (d *document) splitByLines(start, end int, headingPath []string, maxChars, overlapChars int) []DocumentChunk {
	var chunks []DocumentChunk
	windowStart, cursor := start, start
	for cursor <= end {
		if cursor > windowStart && d.sliceLength(windowStart, cursor) > maxChars {
			chunks = append(chunks, d.makeChunk(windowStart, cursor-1, headingPath))
			windowStart = d.overlapStart(cursor, windowStart+1, overlapChars)
			continue
		}
		cursor++
	}
	if windowStart <= end {
		chunks = append(chunks, d.makeChunk(windowStart, end, headingPath))
	}
	return chunks
}

// overlapStart is the first line of the trailing window ending at before-1 that
// fits overlapChars. notBefore guarantees forward progress.
func (d *document) overlapStart(before, notBefore, overlapChars int) int {
	if overlapChars <= 0 || before <= notBefore {
		return before
	}
	candidate := before
	for candidate > notBefore && d.sliceLength(candidate-1, before-1) <= overlapChars {
		candidate--
	}
	return candidate
}

type lineRange struct{ start, end int }

// paragraphs are the contiguous runs of non-blank lines, in order.
func (d *document) paragraphs(start, end int) []lineRange {
	var result []lineRange
	open := 0
	for line := start; line <= end; line++ {
		if d.blank(line) {
			if open != 0 {
				result = append(result, lineRange{open, line - 1})
				open = 0
			}
		} else if open == 0 {
			open = line
		}
	}
	if open != 0 {
		result = append(result, lineRange{open, end})
	}
	return result
}

func (d *document) trimBlankEdges(start, end int) (int, int, bool) {
	first, last := start, end
	for first <= last && d.blank(first) {
		first++
	}
	for last >= first && d.blank(last) {
		last--
	}
	return first, last, first <= last
}

func (d *document) sliceLength(start, end int) int {
	if start > end {
		return 0
	}
	return d.sum[end] - d.sum[start-1] + (end - start)
}

func (d *document) makeChunk(start, end int, headingPath []string) DocumentChunk {
	text := strings.Join(d.lines[start-1:end], "\n")
	return DocumentChunk{
		HeadingPath:   headingPath,
		StartLine:     start,
		EndLine:       end,
		Text:          text,
		ContentSHA256: SHA256Hex(text),
	}
}
