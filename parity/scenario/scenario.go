// Package scenario runs the same request scripts against the Swift and the Go
// server and reports every difference. Each side has its own database cloned
// from one template (see docker-compose.parity.yml), so writes never collide.
package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	// Stream reads only the status and headers: the body is an open event
	// stream (SSE) that never ends, so it is closed unread.
	Stream bool `yaml:"stream"`
}

type Request struct {
	Method  string            `yaml:"method"`
	Path    string            `yaml:"path"`
	JSON    any               `yaml:"json"` // marshalled as the body
	Raw     *string           `yaml:"raw"`  // sent verbatim (malformed-body cases)
	Headers map[string]string `yaml:"headers"`
}

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
		if st.Stream && len(st.Capture) > 0 {
			return fmt.Errorf("step %q: a stream step has no body to capture from", st.Name)
		}
		if st.Request.JSON != nil && st.Request.Raw != nil {
			return fmt.Errorf("step %q: json and raw are exclusive", st.Name)
		}
		for _, table := range st.Snapshot {
			if !tableRe.MatchString(table) {
				return fmt.Errorf("step %q: bad snapshot table %q", st.Name, table)
			}
		}
	}
	return nil
}
