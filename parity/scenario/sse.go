package scenario

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"lumina/parity/normalize"
	"reflect"
	"strings"
	"time"
)

const (
	defaultFramesTimeout = 5 * time.Second
	// maxFrameBytes bounds what one stream step reads, whatever the server sends.
	maxFrameBytes = 4 << 20
)

// Coalesce names the token frames of a stream. Swift's QueryStreamEvent is
// {"type":"token","payload":"hel"} and its reasoning stream's is
// {"type":"token","message":"hel"}, so a step says which.
type Coalesce struct {
	Type  string `yaml:"type"`  // the JSON `type` value of a token frame
	Field string `yaml:"field"` // the JSON string field holding its text
}

// readFrames parses up to limit SSE frames from an open stream and returns them
// as comparable values. The read ends at limit, at the end of the stream, or
// when the request context expires, which is not an error: both servers are
// compared on the frames they sent in the time allowed.
//
// Each frame has its field values, with `id` and JSON `data` normalized, plus
// "fields", the field names in order, each with its colon and any space. So
// `data:{}` and `data: {}` differ even though EventSource treats them alike: the
// iOS and web readers are hand-written.
//
// With coalesce, consecutive token frames become one frame holding the joined
// text, normalized only once joined, so a UUID split across two chunks still masks.
// A token frame that differs from its run in anything but the text starts a new
// run, so a change of shape stays visible. A run counts as one frame, and it ends
// at the next other frame, which is what makes limit mean the same on both servers:
// reading on to that frame isn't comparing it.
func readFrames(r io.Reader, limit int, coalesce *Coalesce, norm *normalize.Normalizer, ignore []string) (any, error) {
	br := bufio.NewReader(io.LimitReader(r, maxFrameBytes))
	frames := []any{}
	var (
		cur *sseFrame
		run *tokenRun
	)
	add := func(f *sseFrame) error {
		if len(frames) >= limit {
			return nil
		}
		v, err := f.value(norm, ignore)
		if err != nil {
			return err
		}
		frames = append(frames, v)
		return nil
	}
	flush := func() error {
		if run == nil {
			return nil
		}
		f, err := run.frame(coalesce)
		run = nil
		if err != nil {
			return err
		}
		return add(f)
	}
	for len(frames) < limit {
		line, err := br.ReadString('\n')
		if err != nil {
			break // deadline, end of stream or byte limit; a partial line isn't a frame
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if cur == nil {
				break
			}
			f := cur
			cur = nil
			if tok := parseToken(f, coalesce); tok != nil {
				if run != nil && run.extend(tok) {
					break
				}
				if err := flush(); err != nil {
					return nil, err
				}
				run = tok
				break
			}
			if err := flush(); err != nil {
				return nil, err
			}
			if err := add(f); err != nil {
				return nil, err
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
	if err := flush(); err != nil {
		return nil, err
	}
	return frames, nil
}

// tokenRun is consecutive token frames with the same shape, and their joined text.
type tokenRun struct {
	first *sseFrame
	body  map[string]any // the first frame's JSON, without the text
	text  strings.Builder
}

// parseToken returns a one-frame run if f is a token frame.
func parseToken(f *sseFrame, c *Coalesce) *tokenRun {
	if c == nil || len(f.data) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(strings.Join(f.data, "\n")))
	dec.UseNumber()
	var body map[string]any
	if dec.Decode(&body) != nil || dec.More() || body["type"] != c.Type {
		return nil
	}
	text, ok := body[c.Field].(string)
	if !ok {
		return nil
	}
	delete(body, c.Field)
	run := &tokenRun{first: f, body: body}
	run.text.WriteString(text)
	return run
}

// extend adds next's text if only the text differs, and reports whether it did.
func (r *tokenRun) extend(next *tokenRun) bool {
	if !reflect.DeepEqual(r.body, next.body) || !reflect.DeepEqual(r.first.fields, next.first.fields) || !reflect.DeepEqual(r.first.values, next.first.values) {
		return false
	}
	r.text.WriteString(next.text.String())
	return true
}

// frame is the run as one frame.
func (r *tokenRun) frame(c *Coalesce) (*sseFrame, error) {
	body := make(map[string]any, len(r.body)+1)
	for k, v := range r.body {
		body[k] = v
	}
	body[c.Field] = r.text.String()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, err
	}
	joined := *r.first
	joined.data = []string{strings.TrimSpace(buf.String())}
	return &joined, nil
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
