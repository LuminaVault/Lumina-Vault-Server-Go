package scenario

import (
	"bufio"
	"io"
	"lumina/parity/normalize"
	"strings"
	"time"
)

const (
	defaultFramesTimeout = 5 * time.Second
	// maxFrameBytes bounds what one stream step reads, whatever the server sends.
	maxFrameBytes = 4 << 20
)

// readFrames parses up to limit SSE frames from an open stream and returns them
// as comparable values. The read ends at limit, at the end of the stream, or
// when the request context expires, which is not an error: both servers are
// compared on the frames they sent in the time allowed.
//
// Each frame has its field values, with `id` and JSON `data` normalized, plus
// "fields", the field names in order, each with its colon and any space. So
// `data:{}` and `data: {}` differ even though EventSource treats them alike: the
// iOS and web readers are hand-written.
func readFrames(r io.Reader, limit int, norm *normalize.Normalizer, ignore []string) (any, error) {
	br := bufio.NewReader(io.LimitReader(r, maxFrameBytes))
	frames := []any{}
	var cur *sseFrame
	for len(frames) < limit {
		line, err := br.ReadString('\n')
		if err != nil {
			break // deadline, end of stream or byte limit; a partial line isn't a frame
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if cur != nil {
				v, err := cur.value(norm, ignore)
				if err != nil {
					return nil, err
				}
				frames = append(frames, v)
				cur = nil
			}
		case strings.HasPrefix(line, ":"):
			// A comment: a keep-alive whose timing isn't part of the contract.
		default:
			if cur == nil {
				cur = &sseFrame{}
			}
			cur.add(line)
		}
	}
	return frames, nil
}

type sseFrame struct {
	fields []string
	values map[string]string
	data   []string
}

func (f *sseFrame) add(line string) {
	name, value, hasColon := strings.Cut(line, ":")
	shape := name
	if hasColon {
		shape += ":"
		if strings.HasPrefix(value, " ") {
			shape += " "
			value = value[1:]
		}
	}
	f.fields = append(f.fields, shape)
	if name == "data" {
		f.data = append(f.data, value)
		return
	}
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[name] = value
}

func (f *sseFrame) value(norm *normalize.Normalizer, ignore []string) (any, error) {
	out := map[string]any{"fields": strings.Join(f.fields, ",")}
	for k, v := range f.values {
		out[k] = norm.Value(v)
	}
	if f.data != nil {
		data, err := norm.Body([]byte(strings.Join(f.data, "\n")), ignore)
		if err != nil {
			return nil, err
		}
		out["data"] = data
	}
	return out, nil
}
