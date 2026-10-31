package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"lumina/internal/auth"
	"lumina/internal/ids"
	"lumina/parity/normalize"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	tableRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	varRe   = regexp.MustCompile(`\{\{([A-Za-z0-9_]+)\}\}`)
)

// Snapshotter returns every row of a table as JSON, in a stable order.
type Snapshotter func(ctx context.Context, table string) ([]json.RawMessage, error)

// Target is one server under test.
type Target struct {
	Name     string // "swift" or "go"
	BaseURL  string
	Keys     auth.Keyset
	Users    map[string]uuid.UUID
	Snapshot Snapshotter  // nil disables snapshots (smoke mode)
	Token    string       // when set, used for every authenticated step (smoke mode)
	Client   *http.Client // nil uses a 30 s default
}

type Transcript struct {
	Scenario string       `json:"scenario"`
	Target   string       `json:"target"`
	Steps    []StepResult `json:"steps"`
}

type StepResult struct {
	Name        string            `json:"name"`
	Status      int               `json:"status"`
	ContentType string            `json:"contentType"`
	Body        any               `json:"body"`
	Headers     map[string]string `json:"headers,omitempty"`
	Snapshots   map[string]any    `json:"snapshots,omitempty"`
}

// Run executes sc against t. One Normalizer spans the whole run, so an id
// created in step 1 has the same placeholder wherever it appears later.
func Run(ctx context.Context, sc Scenario, t Target) (Transcript, error) {
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	norm := normalize.New()
	vars := map[string]string{}
	tr := Transcript{Scenario: sc.File, Target: t.Name}

	for _, st := range sc.Steps {
		req, err := buildRequest(ctx, t, st, vars)
		if err != nil {
			return tr, fmt.Errorf("%s/%s: %w", sc.File, st.Name, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return tr, fmt.Errorf("%s/%s: %w", sc.File, st.Name, err)
		}
		var raw []byte
		if !st.Stream {
			raw, err = io.ReadAll(resp.Body)
		}
		_ = resp.Body.Close()
		if err != nil {
			return tr, err
		}
		if err := capture(raw, st.Capture, vars); err != nil {
			return tr, fmt.Errorf("%s/%s: %w", sc.File, st.Name, err)
		}
		body, err := norm.Body(raw, append(append([]string{}, sc.Ignore...), st.Ignore...))
		if err != nil {
			return tr, err
		}
		res := StepResult{Name: st.Name, Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body}
		for _, h := range st.Headers {
			if res.Headers == nil {
				res.Headers = map[string]string{}
			}
			res.Headers[h] = strings.Join(resp.Header.Values(h), ", ")
		}
		if t.Snapshot != nil && len(st.Snapshot) > 0 {
			res.Snapshots = map[string]any{}
			for _, table := range st.Snapshot {
				rows, err := t.Snapshot(ctx, table)
				if err != nil {
					return tr, fmt.Errorf("%s/%s snapshot %s: %w", sc.File, st.Name, table, err)
				}
				stableOrder(rows)
				vals := make([]any, len(rows))
				for i, r := range rows {
					if vals[i], err = norm.Body(r, nil); err != nil {
						return tr, err
					}
				}
				res.Snapshots[table] = vals
			}
		}
		tr.Steps = append(tr.Steps, res)
	}
	return tr, nil
}

func buildRequest(ctx context.Context, t Target, st Step, vars map[string]string) (*http.Request, error) {
	var body io.Reader
	contentType := ""
	switch {
	case st.Request.Raw != nil:
		body = strings.NewReader(substitute(*st.Request.Raw, vars))
		contentType = "application/json"
	case st.Request.JSON != nil:
		b, err := json.Marshal(st.Request.JSON)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(substitute(string(b), vars))
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, st.Request.Method, strings.TrimRight(t.BaseURL, "/")+substitute(st.Request.Path, vars), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range st.Request.Headers {
		req.Header.Set(k, substitute(v, vars))
	}
	if st.As != "" {
		token := t.Token
		if token == "" {
			user, ok := t.Users[st.As]
			if !ok {
				return nil, fmt.Errorf("unknown fixture user %q", st.As)
			}
			if token, err = Mint(t.Keys, user); err != nil {
				return nil, err
			}
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// stableOrder sorts snapshot rows so both servers list them identically even
// though their ids are random: by created_at (both ran the same requests in the
// same order), then by the row with random values masked.
func stableOrder(rows []json.RawMessage) {
	type keyed struct {
		created, masked string
		row             json.RawMessage
	}
	ks := make([]keyed, len(rows))
	for i, r := range rows {
		var m map[string]any
		_ = json.Unmarshal(r, &m)
		created, _ := m["created_at"].(string)
		masked, _ := json.Marshal(normalize.New().Value(m))
		ks[i] = keyed{created, string(masked), r}
	}
	sort.SliceStable(ks, func(i, j int) bool {
		if ks[i].created != ks[j].created {
			return ks[i].created < ks[j].created
		}
		return ks[i].masked < ks[j].masked
	})
	for i := range ks {
		rows[i] = ks[i].row
	}
}

// substitute replaces {{var}}; an unknown var is left in place so the request
// fails visibly rather than silently hitting the wrong id.
func substitute(s string, vars map[string]string) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := vars[varRe.FindStringSubmatch(m)[1]]; ok {
			return v
		}
		return m
	})
}

func capture(raw []byte, captures map[string]string, vars map[string]string) error {
	if len(captures) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("capture needs a JSON body: %w", err)
	}
	for name, path := range captures {
		v, err := normalize.Get(doc, path)
		if err != nil {
			return fmt.Errorf("capture %s: %w", name, err)
		}
		vars[name] = fmt.Sprint(v)
	}
	return nil
}

// Mint signs a session token the way Swift's SessionToken does (uppercase
// sub and jti, HS256 with the active kid). Only the parity harness mints in Go
// before wave 8.
func Mint(keys auth.Keyset, user uuid.UUID) (string, error) {
	key := keys.Active()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{RegisteredClaims: jwt.RegisteredClaims{
		Subject:   ids.Upper(user),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(now),
		ID:        ids.Upper(uuid.New()),
	}})
	if key.Kid != "" {
		tok.Header["kid"] = key.Kid
	}
	return tok.SignedString(key.Secret)
}

// JSONBody is a small helper for transcripts written to disk.
func (t Transcript) JSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	err := enc.Encode(t)
	return buf.Bytes(), err
}
