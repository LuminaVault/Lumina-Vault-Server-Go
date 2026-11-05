package scenario

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	Files    FileReader   // nil fails a step with `files`, except in smoke mode
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
	// Files holds each tree of the step's `files`.
	Files map[string]any `json:"files,omitempty"`
	// TriggerStatus is the status of the step's `trigger` request.
	TriggerStatus int `json:"triggerStatus,omitempty"`
}

// Run executes sc against t. One Normalizer spans the whole run, so an id
// created in step 1 has the same placeholder wherever it appears later.
func Run(ctx context.Context, sc Scenario, t Target) (Transcript, error) {
	client := t.Client
	if client == nil {
		// Redirects are compared, not followed (the calendar OAuth callback answers
		// 303 to a luminavault:// URL a Go client cannot fetch).
		client = &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	norm := normalize.New()
	vars := map[string]string{}
	tr := Transcript{Scenario: sc.File, Target: t.Name}

	for _, st := range sc.Steps {
		res, err := runStep(ctx, sc, st, t, client, norm, vars)
		if err != nil {
			return tr, fmt.Errorf("%s/%s: %w", sc.File, st.Name, err)
		}
		tr.Steps = append(tr.Steps, res)
	}
	return tr, nil
}

func runStep(ctx context.Context, sc Scenario, st Step, t Target, client *http.Client, norm *normalize.Normalizer, vars map[string]string) (StepResult, error) {
	var res StepResult
	ignore := append(append([]string{}, sc.Ignore...), st.Ignore...)
	reqCtx := ctx
	if st.Frames > 0 {
		timeout := st.FramesTimeout
		if timeout == 0 {
			timeout = defaultFramesTimeout
		}
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := buildRequest(reqCtx, t, st.As, st.Request, vars)
	if err != nil {
		return res, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return res, err
	}
	defer func() { _ = resp.Body.Close() }()

	var (
		raw        []byte
		body       any
		triggerRes int
	)
	switch {
	case st.Frames > 0:
		// Smoke checks statuses on a deployed server, so it doesn't need the write.
		if st.Trigger != nil && t.Token == "" {
			if triggerRes, err = sendTrigger(ctx, t, client, *st.Trigger, vars); err != nil {
				return res, fmt.Errorf("trigger: %w", err)
			}
		}
		if body, err = readFrames(resp.Body, st.Frames, norm, ignore); err != nil {
			return res, err
		}
	case st.Stream:
		body = ""
	default:
		if raw, err = io.ReadAll(resp.Body); err != nil {
			return res, err
		}
		if err := capture(raw, st.Capture, vars); err != nil {
			return res, err
		}
		if st.Binary {
			body = binaryBody(raw)
		} else if st.Zip && resp.StatusCode == http.StatusOK {
			body = zipBody(raw, norm)
		} else if body, err = norm.Body(raw, ignore); err != nil {
			return res, err
		}
	}
	_ = resp.Body.Close()

	res = StepResult{Name: st.Name, Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body, TriggerStatus: triggerRes}
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
				return res, fmt.Errorf("snapshot %s: %w", table, err)
			}
			stableOrder(rows)
			vals := make([]any, len(rows))
			for i, r := range rows {
				// UUIDs first: a column may embed a random id, such as
				// analytics_events' "memory-created:<id>" key.
				if vals[i], err = norm.Body([]byte(norm.UUIDs(string(r))), nil); err != nil {
					return res, err
				}
			}
			res.Snapshots[table] = vals
		}
	}
	if res.Files, err = snapshotFiles(ctx, st, t, norm, vars); err != nil {
		return res, err
	}
	return res, nil
}

// sendTrigger sends a stream step's trigger and returns its status.
func sendTrigger(ctx context.Context, t Target, client *http.Client, tr Trigger, vars map[string]string) (int, error) {
	req, err := buildRequest(ctx, t, tr.As, tr.Request, vars)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// binaryBody is what a `binary` step compares.
func binaryBody(raw []byte) any {
	sum := sha256.Sum256(raw)
	return map[string]any{"size": len(raw), "sha256": hex.EncodeToString(sum[:])}
}

func buildRequest(ctx context.Context, t Target, as string, r Request, vars map[string]string) (*http.Request, error) {
	var body io.Reader
	contentType := ""
	switch {
	case r.Raw != nil:
		body = strings.NewReader(substitute(*r.Raw, vars))
		contentType = "application/json"
	case r.JSON != nil:
		b, err := json.Marshal(r.JSON)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(substitute(string(b), vars))
		contentType = "application/json"
	case r.Base64 != nil:
		b, err := base64.StdEncoding.DecodeString(*r.Base64)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	case r.Fill != nil:
		repeat := r.Fill.Size/len(r.Fill.Text) + 1
		body = strings.NewReader(strings.Repeat(r.Fill.Text, repeat)[:r.Fill.Size])
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, strings.TrimRight(t.BaseURL, "/")+substitute(r.Path, vars), body)
	if err != nil {
		return nil, err
	}
	if r.Chunked {
		req.ContentLength = -1
		req.TransferEncoding = []string{"chunked"}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range r.Headers {
		req.Header.Set(k, substitute(v, vars))
	}
	if as != "" {
		token := t.Token
		if token == "" {
			user, ok := t.Users[as]
			if !ok {
				return nil, fmt.Errorf("unknown fixture user %q", as)
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
// though their ids are random: by the time the row was written (both ran the same
// requests in the same order), then by the row with random values masked.
func stableOrder(rows []json.RawMessage) {
	type keyed struct {
		created, masked string
		row             json.RawMessage
	}
	ks := make([]keyed, len(rows))
	for i, r := range rows {
		var m map[string]any
		_ = json.Unmarshal(r, &m)
		// Tables with no created_at: analytics_events, memory_sync_tombstones.
		created, _ := m["created_at"].(string)
		for _, column := range []string{"occurred_at", "deleted_at"} {
			if created == "" {
				created, _ = m[column].(string)
			}
		}
		n := normalize.New()
		masked, _ := json.Marshal(n.Value(m))
		ks[i] = keyed{created, n.UUIDs(string(masked)), r}
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
			return fmt.Errorf("capture %s: %w, in response %.300s", name, err, raw)
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
