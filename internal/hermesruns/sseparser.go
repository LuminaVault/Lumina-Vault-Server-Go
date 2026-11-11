package hermesruns

import "strings"

// Record is SSEFrameParser.Record.
type Record struct {
	Event *string
	Data  string
}

// Parser is SSEFrameParser: incremental WHATWG framing. Records end at the first
// blank line; comment lines are dropped and multi-line data joins with "\n".
type Parser struct {
	buffer string
}

// Feed appends text and returns every complete record.
func (p *Parser) Feed(text string) []Record {
	p.buffer += text
	var out []Record
	for {
		start, end, ok := recordTerminator(p.buffer)
		if !ok {
			return out
		}
		raw := p.buffer[:start]
		p.buffer = p.buffer[end:]
		if r, ok := ParseRecord(raw); ok {
			out = append(out, r)
		}
	}
}

// Flush parses whatever is left, a record the upstream never terminated.
func (p *Parser) Flush() (Record, bool) {
	raw := p.buffer
	p.buffer = ""
	return ParseRecord(raw)
}

func recordTerminator(text string) (start, end int, ok bool) {
	best := -1
	bestLen := 0
	for _, candidate := range []string{"\r\n\r\n", "\n\n", "\r\r"} {
		i := strings.Index(text, candidate)
		if i < 0 {
			continue
		}
		if best >= 0 && best <= i {
			continue
		}
		best, bestLen = i, len(candidate)
	}
	if best < 0 {
		return 0, 0, false
	}
	return best, best + bestLen, true
}

// ParseRecord is SSEFrameParser.parse(record:). Swift splits on "\n" (a "\r\n"
// grapheme counts as one separator too) and trims "\r" from each line.
func ParseRecord(record string) (Record, bool) {
	var event *string
	var data []string
	for _, raw := range strings.Split(strings.ReplaceAll(record, "\r\n", "\n"), "\n") {
		line := strings.Trim(raw, "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		field, value := line, ""
		if i := strings.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], strings.TrimPrefix(line[i+1:], " ")
		}
		switch field {
		case "event":
			v := value
			event = &v
		case "data":
			data = append(data, value)
		}
	}
	if len(data) == 0 {
		return Record{}, false
	}
	return Record{Event: event, Data: strings.Join(data, "\n")}, true
}
