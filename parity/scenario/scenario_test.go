package scenario

import (
	"context"
	"encoding/json"
	"io"
	"lumina/internal/auth"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const secret = "parity-secret-parity-secret-0000"

var (
	keys  = auth.NewKeyset(auth.Key{Kid: "parity", Secret: []byte(secret)})
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
)

// fakeServer answers POST /v1/todos with a created todo and
// GET /v1/todos/{id} with that id; knobs change one aspect of the response.
type knobs struct {
	status      int
	title       string
	contentType string
	lowerIDs    bool
}

func fakeServer(t *testing.T, k knobs) *httptest.Server {
	t.Helper()
	id := "C0FFEE00-0000-4000-8000-0000000000AA"
	if k.lowerIDs {
		id = strings.ToLower(id)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.Verify(keys, r.Header.Get("Authorization"))
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, strings.ToUpper(alice.String()), claims.Subject)
		ct := k.contentType
		if ct == "" {
			ct = "application/json; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		switch {
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(b, &in)
			title := in["title"].(string)
			if k.title != "" {
				title = k.title
			}
			status := http.StatusCreated
			if k.status != 0 {
				status = k.status
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": title, "createdAt": "2026-10-29T10:00:00Z"})
		case r.URL.Path == "/v1/todos/"+id:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

var sample = Scenario{File: "crud.yaml", Module: "todos", Steps: []Step{
	{Name: "create", As: "alice", Request: Request{Method: "POST", Path: "/v1/todos", JSON: map[string]any{"title": "Buy milk"}}, Capture: map[string]string{"todoId": "$.id"}, Snapshot: []string{"memories"}},
	{Name: "get", As: "alice", Request: Request{Method: "GET", Path: "/v1/todos/{{todoId}}"}},
}}

func run(t *testing.T, srv *httptest.Server, snapshotRows string) Transcript {
	t.Helper()
	tr, err := Run(context.Background(), sample, Target{
		Name: "x", BaseURL: srv.URL, Keys: keys, Users: map[string]uuid.UUID{"alice": alice},
		Snapshot: func(context.Context, string) ([]json.RawMessage, error) {
			return []json.RawMessage{json.RawMessage(snapshotRows)}, nil
		},
	})
	require.NoError(t, err)
	return tr
}

func TestIdenticalServersHaveNoDiff(t *testing.T) {
	a, b := fakeServer(t, knobs{}), fakeServer(t, knobs{})
	defer a.Close()
	defer b.Close()
	require.Empty(t, Diff(run(t, a, `{"n":1}`), run(t, b, `{"n":1}`)))
}

func TestDiffDetectsStatusBodyAndSnapshot(t *testing.T) {
	base := fakeServer(t, knobs{})
	defer base.Close()
	want := run(t, base, `{"n":1}`)

	for name, tc := range map[string]struct {
		k     knobs
		rows  string
		field string
	}{
		"status":       {knobs{status: http.StatusOK}, `{"n":1}`, "status"},
		"body":         {knobs{title: "Buy bread"}, `{"n":1}`, "$.title"},
		"content-type": {knobs{contentType: "text/plain"}, `{"n":1}`, "content-type"},
		"uuid case":    {knobs{lowerIDs: true}, `{"n":1}`, "$.id"},
		"snapshot":     {knobs{}, `{"n":2}`, "snapshot:memories$[0].n"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := fakeServer(t, tc.k)
			defer srv.Close()
			diffs := Diff(want, run(t, srv, tc.rows))
			require.NotEmpty(t, diffs)
			fields := []string{}
			for _, d := range diffs {
				fields = append(fields, d.Field)
			}
			require.Contains(t, fields, tc.field)
		})
	}
}

func TestCaptureSubstitution(t *testing.T) {
	srv := fakeServer(t, knobs{})
	defer srv.Close()
	tr := run(t, srv, `{}`)
	require.Equal(t, http.StatusOK, tr.Steps[1].Status, "GET must use the captured id")
	require.Equal(t, map[string]any{"id": "<uuid#1:upper>"}, tr.Steps[1].Body)
}

func TestMintIsVerifiable(t *testing.T) {
	tok, err := Mint(keys, alice)
	require.NoError(t, err)
	claims, err := auth.Verify(keys, "Bearer "+tok)
	require.NoError(t, err)
	require.Equal(t, "A11CE000-0000-4000-8000-000000000001", claims.Subject)
}

func TestLoadValidates(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("a.yaml", "module: todos\nsteps:\n  - name: list\n    as: alice\n    request: {method: GET, path: /v1/todos}\n")
	scs, err := Load(dir)
	require.NoError(t, err)
	require.Len(t, scs, 1)
	require.Equal(t, "a.yaml", scs[0].File)

	write("b.yaml", "module: todos\nsteps:\n  - name: bad\n    request: {method: GET, path: /x}\n    snapshot: [\"todos; drop table users\"]\n")
	_, err = Load(dir)
	require.ErrorContains(t, err, "bad snapshot table")

	require.NoError(t, os.Remove(filepath.Join(dir, "b.yaml")))
	write("c.yaml", "module: todos\nstepz: []\n")
	_, err = Load(dir)
	require.Error(t, err, "unknown fields are rejected")
}

// A stream step returns once the headers arrive, without waiting for the
// never-ending SSE body, and compares the listed headers.
func TestStreamStepReadsHeadersOnly(t *testing.T) {
	sse := func(cache string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", cache)
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
	}
	sc := Scenario{File: "s.yaml", Module: "teams", Steps: []Step{
		{Name: "stream", As: "alice", Request: Request{Method: "GET", Path: "/stream"}, Stream: true, Headers: []string{"Cache-Control"}},
	}}
	runOn := func(srv *httptest.Server) Transcript {
		tr, err := Run(context.Background(), sc, Target{
			Name: "x", BaseURL: srv.URL, Keys: keys, Users: map[string]uuid.UUID{"alice": alice},
			Client: &http.Client{Timeout: 5 * time.Second},
		})
		require.NoError(t, err)
		return tr
	}
	a, b := sse("no-cache"), sse("max-age=1")
	defer a.Close()
	defer b.Close()
	ta := runOn(a)
	require.Equal(t, http.StatusOK, ta.Steps[0].Status)
	require.Equal(t, "text/event-stream", ta.Steps[0].ContentType)
	require.Equal(t, map[string]string{"Cache-Control": "no-cache"}, ta.Steps[0].Headers)
	require.Empty(t, Diff(ta, runOn(a)))
	diffs := Diff(ta, runOn(b))
	require.Len(t, diffs, 1)
	require.Equal(t, "header:Cache-Control", diffs[0].Field)
}
