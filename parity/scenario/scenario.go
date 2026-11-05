// Package scenario runs the same request scripts against the Swift and the Go
// server and reports every difference. Each side has its own database cloned
// from one template (see docker-compose.parity.yml), so writes never collide.
package scenario

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Scenario is one YAML file under parity/scenarios/<module>/.
type Scenario struct {
	File   string   `yaml:"-"`
	Module string   `yaml:"module"`
	Smoke  bool     `yaml:"smoke"`  // also run against deployed staging (G2)
	Ignore []string `yaml:"ignore"` // applied to every step
	Steps  []Step   `yaml:"steps"`
}

type Step struct {
	Name     string            `yaml:"name"`
	As       string            `yaml:"as"` // fixture user; "" sends no Authorization
	Request  Request           `yaml:"request"`
	Capture  map[string]string `yaml:"capture"`  // var → JSON path into the response
	Snapshot []string          `yaml:"snapshot"` // tables compared after the step
	Ignore   []string          `yaml:"ignore"`
	// Headers lists response headers to compare, besides Content-Type.
	Headers []string `yaml:"headers"`
	// Binary compares the response body by size and sha256 rather than as text
	// (downloads, exports).
	Binary bool `yaml:"binary"`
	// Zip compares a zip body entry by entry (see zip.go): names, flags, sizes and
	// normalized contents, and the archive's size. Entry times are left out, and
	// so is the raw sha `binary` compares, because an archive embeds both.
	Zip bool `yaml:"zip"`
	// Stream reads only the status and headers: the body is an open event
	// stream (SSE) that never ends, so it is closed unread, unless Frames is set.
	Stream bool `yaml:"stream"`
	// Frames, with Stream, reads up to this many SSE frames and compares them.
	// Comment lines (keep-alives) don't count and aren't compared.
	Frames int `yaml:"frames"`
	// FramesTimeout bounds the whole step (default 5 s). Fewer frames than Frames
	// by then is not an error: the frames read are what gets compared.
	FramesTimeout time.Duration `yaml:"framesTimeout"`
	// Trigger is sent once the stream is open, for streams with no initial frame.
	Trigger *Trigger `yaml:"trigger"`
	// Files lists vault trees compared after the step (see files.go): a fixture
	// user, a UUID, or a {{var}} holding one, read as tenants/<UPPERCASE UUID>.
	Files []string `yaml:"files"`
	// FilesMask holds regexes blanked to <mask> in paths, for values no UUID
	// placeholder covers, such as the Unix time in `_deleted_<seconds>_<slug>`.
	// With FilesNormalizeContent they apply to file contents too.
	FilesMask []string `yaml:"filesMask"`
	// FilesNormalizeContent replaces UUIDs and timestamps in file contents with
	// placeholders before hashing, for files that embed server-generated values.
	FilesNormalizeContent bool `yaml:"filesNormalizeContent"`
}

// Trigger is a request sent while a stream step reads its frames.
type Trigger struct {
	As      string  `yaml:"as"`
	Request Request `yaml:"request"`
}

// Request describes one HTTP request. At most one of JSON, Raw, Base64 and Fill
// sets the body. Only JSON and Raw set a default Content-Type.
type Request struct {
	Method  string            `yaml:"method"`
	Path    string            `yaml:"path"`
	JSON    any               `yaml:"json"`   // marshalled as the body
	Raw     *string           `yaml:"raw"`    // sent verbatim (malformed-body cases)
	Base64  *string           `yaml:"base64"` // decoded and sent (binary uploads)
	Fill    *Fill             `yaml:"fill"`   // a generated body (limits, large uploads)
	Chunked bool              `yaml:"chunked"`
	Headers map[string]string `yaml:"headers"`
}

// Fill is Text repeated and cut to exactly Size bytes.
type Fill struct {
	Text string `yaml:"text"`
	Size int    `yaml:"size"`
}

// maxFill keeps a scenario's generated body well under the runner's memory.
const maxFill = 64 << 20

// maxFramesTimeout stays under the runner's default 30 s client timeout.
const maxFramesTimeout = 25 * time.Second

// Load reads every *.yaml in dir, sorted by file name.
func Load(dir string) ([]Scenario, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	out := make([]Scenario, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var sc Scenario
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&sc); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		sc.File = filepath.Base(f)
		if err := sc.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, sc)
	}
	return out, nil
}

func (sc Scenario) validate() error {
	if sc.Module == "" {
		return fmt.Errorf("module is required")
	}
	for i, st := range sc.Steps {
		if st.Name == "" || st.Request.Method == "" || st.Request.Path == "" {
			return fmt.Errorf("step %d needs name, request.method and request.path", i)
		}
		if err := st.validate(); err != nil {
			return fmt.Errorf("step %q: %w", st.Name, err)
		}
	}
	return nil
}

func (st Step) validate() error {
	if st.Stream && len(st.Capture) > 0 {
		return errors.New("a stream step has no body to capture from")
	}
	if st.Binary && (st.Stream || len(st.Capture) > 0) {
		return errors.New("binary is exclusive with stream and capture")
	}
	if err := st.Request.validate(); err != nil {
		return err
	}
	for _, table := range st.Snapshot {
		if !tableRe.MatchString(table) {
			return fmt.Errorf("bad snapshot table %q", table)
		}
	}
	if st.Frames < 0 || (st.Frames > 0 && !st.Stream) {
		return errors.New("frames needs stream and a positive count")
	}
	if st.Frames == 0 && (st.FramesTimeout != 0 || st.Trigger != nil) {
		return errors.New("framesTimeout and trigger need frames")
	}
	if st.FramesTimeout < 0 || st.FramesTimeout > maxFramesTimeout {
		return fmt.Errorf("framesTimeout must be at most %s", maxFramesTimeout)
	}
	if st.Trigger != nil {
		if st.Trigger.Request.Method == "" || st.Trigger.Request.Path == "" {
			return errors.New("trigger needs request.method and request.path")
		}
		if err := st.Trigger.Request.validate(); err != nil {
			return fmt.Errorf("trigger: %w", err)
		}
	}
	if len(st.Files) == 0 && (len(st.FilesMask) > 0 || st.FilesNormalizeContent) {
		return errors.New("filesMask and filesNormalizeContent need files")
	}
	for _, m := range st.FilesMask {
		if _, err := regexp.Compile(m); err != nil {
			return fmt.Errorf("bad filesMask: %w", err)
		}
	}
	return nil
}

func (r Request) validate() error {
	bodies := 0
	if r.JSON != nil {
		bodies++
	}
	if r.Raw != nil {
		bodies++
	}
	if r.Base64 != nil {
		bodies++
		if _, err := base64.StdEncoding.DecodeString(*r.Base64); err != nil {
			return fmt.Errorf("bad base64 body: %w", err)
		}
	}
	if r.Fill != nil {
		bodies++
		if r.Fill.Text == "" || r.Fill.Size <= 0 || r.Fill.Size > maxFill {
			return fmt.Errorf("fill needs text and a size from 1 to %d", maxFill)
		}
	}
	if bodies > 1 {
		return errors.New("json, raw, base64 and fill are exclusive")
	}
	if r.Chunked && bodies == 0 {
		return errors.New("chunked needs a body")
	}
	return nil
}
