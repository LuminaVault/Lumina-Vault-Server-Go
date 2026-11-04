package scenario

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"lumina/parity/normalize"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func scenarioOf(steps ...Step) Scenario { return Scenario{File: "t.yaml", Module: "t", Steps: steps} }

func target(url string) Target {
	return Target{Name: "t", BaseURL: url, Keys: keys, Users: map[string]uuid.UUID{"alice": alice}}
}

func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func diffOf(t *testing.T, sc Scenario, a, b Target) []Difference {
	t.Helper()
	x, err := Run(context.Background(), sc, a)
	require.NoError(t, err)
	y, err := Run(context.Background(), sc, b)
	require.NoError(t, err)
	return Diff(x, y)
}

func TestRequestBodies(t *testing.T) {
	type seen struct {
		body        []byte
		length      int64
		chunked     bool
		contentType string
		metadata    string
		query       string
	}
	var got seen
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = seen{body, r.ContentLength, len(r.TransferEncoding) > 0, r.Header.Get("Content-Type"), r.Header.Get("X-Vault-Metadata"), r.URL.RawQuery}
		w.WriteHeader(http.StatusNoContent)
	})
	run := func(r Request) seen {
		r.Method, r.Path = http.MethodPut, "/x"
		_, err := Run(context.Background(), scenarioOf(Step{Name: "s", Request: r}), target(srv.URL))
		require.NoError(t, err)
		return got
	}

	binary := []byte{0x00, 0xFF, 0xFE, 'a', 0x80}
	encoded := base64.StdEncoding.EncodeToString(binary)
	s := run(Request{Base64: &encoded})
	require.Equal(t, binary, s.body)
	require.Equal(t, int64(len(binary)), s.length)
	require.False(t, s.chunked)
	require.Empty(t, s.contentType, "only json and raw set a default")

	s = run(Request{Fill: &Fill{Text: "abc", Size: 7}})
	require.Equal(t, "abcabca", string(s.body))
	require.Equal(t, int64(7), s.length)

	s = run(Request{Fill: &Fill{Text: "x", Size: 1 << 16}, Chunked: true})
	require.Len(t, s.body, 1<<16)
	require.True(t, s.chunked)
	require.Equal(t, int64(-1), s.length)

	s = run(Request{Base64: &encoded, Path: "/ignored", Headers: map[string]string{"Content-Type": "image/png", "X-Vault-Metadata": `{"a":1}`}})
	require.Equal(t, "image/png", s.contentType)
	require.Equal(t, `{"a":1}`, s.metadata)
}

func TestLiteralPlusReachesTheServer(t *testing.T) {
	var raw string
	srv := serve(t, func(_ http.ResponseWriter, r *http.Request) { raw = r.URL.RawQuery })
	_, err := Run(context.Background(), scenarioOf(Step{Name: "s", Request: Request{Method: http.MethodGet, Path: "/x?a=1+2&b=%2B&c=100%"}}), target(srv.URL))
	require.NoError(t, err)
	require.Equal(t, "a=1+2&b=%2B&c=100%", raw)
}

func TestBinaryBodiesCompareByBytes(t *testing.T) {
	bytesServer := func(b []byte) *httptest.Server {
		return serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(b)
		})
	}
	sc := scenarioOf(Step{Name: "dl", Binary: true, Request: Request{Method: http.MethodGet, Path: "/x"}})
	a := bytesServer([]byte{0xFF, 0x00, 0x01})
	require.Empty(t, diffOf(t, sc, target(a.URL), target(bytesServer([]byte{0xFF, 0x00, 0x01}).URL)))

	diffs := diffOf(t, sc, target(a.URL), target(bytesServer([]byte{0xFF, 0x00, 0x02}).URL)) // same size
	require.Len(t, diffs, 1)
	require.Equal(t, "$.sha256", diffs[0].Field)

	diffs = diffOf(t, sc, target(a.URL), target(bytesServer([]byte{0xFF}).URL))
	require.Len(t, diffs, 2)
}

func TestHeadComparesHeadersWithNoBody(t *testing.T) {
	headServer := func(length string) *httptest.Server {
		return serve(t, func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodHead, r.Method)
			w.Header().Set("Content-Length", length)
			w.Header().Set("Accept-Ranges", "bytes")
		})
	}
	sc := scenarioOf(Step{Name: "head", Headers: []string{"Content-Length", "Accept-Ranges"}, Request: Request{Method: http.MethodHead, Path: "/x"}})
	a := headServer("1024")
	require.Empty(t, diffOf(t, sc, target(a.URL), target(headServer("1024").URL)))
	diffs := diffOf(t, sc, target(a.URL), target(headServer("2048").URL))
	require.Len(t, diffs, 1)
	require.Equal(t, "header:Content-Length", diffs[0].Field)
}

func sseServer(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			_ = rc.Flush()
		}
		<-r.Context().Done() // never ends on its own
	})
}

func framesStep(n int) Step {
	return Step{Name: "s", Stream: true, Frames: n, FramesTimeout: 300 * time.Millisecond, Request: Request{Method: http.MethodGet, Path: "/s"}}
}

func TestSSEFramesAreCompared(t *testing.T) {
	const idA, idB = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	frame := func(id string) string {
		return "id: " + id + "\nevent: activity\ndata: {\"id\":\"" + id + "\",\"at\":\"2026-01-01T00:00:00Z\",\"n\":1}\n\n"
	}
	a := sseServer(t, ": connected\n\n", frame(idA), ": keep-alive\n\n", "data: done\n\n", "data: not read\n\n")

	t.Run("random ids and comments are not differences", func(t *testing.T) {
		b := sseServer(t, frame(idB), "data: done\n\n", ": late\n\n")
		require.Empty(t, diffOf(t, scenarioOf(framesStep(2)), target(a.URL), target(b.URL)))
	})
	t.Run("a value differs", func(t *testing.T) {
		b := sseServer(t, frame(idB), "data: other\n\n")
		diffs := diffOf(t, scenarioOf(framesStep(2)), target(a.URL), target(b.URL))
		require.Len(t, diffs, 1)
		require.Equal(t, "$[1].data", diffs[0].Field)
	})
	t.Run("the wire spelling differs", func(t *testing.T) {
		b := sseServer(t, "id:"+idB+"\nevent: activity\ndata: {\"id\":\""+idB+"\",\"at\":\"2026-01-01T00:00:00Z\",\"n\":1}\n\n", "data: done\n\n")
		diffs := diffOf(t, scenarioOf(framesStep(2)), target(a.URL), target(b.URL))
		require.Len(t, diffs, 1)
		require.Equal(t, "$[0].fields", diffs[0].Field)
	})
	t.Run("a missing frame shows at the timeout", func(t *testing.T) {
		b := sseServer(t, frame(idB))
		start := time.Now()
		diffs := diffOf(t, scenarioOf(framesStep(2)), target(a.URL), target(b.URL))
		require.Less(t, time.Since(start), 5*time.Second)
		require.Equal(t, "$.length", diffs[0].Field)
	})
	t.Run("the read stops at the limit", func(t *testing.T) {
		tr, err := Run(context.Background(), scenarioOf(framesStep(1)), target(a.URL))
		require.NoError(t, err)
		require.Len(t, tr.Steps[0].Body, 1)
	})
	t.Run("ignore paths reach frame data", func(t *testing.T) {
		b := sseServer(t, "id: "+idB+"\nevent: activity\ndata: {\"id\":\""+idB+"\",\"at\":\"2026-01-01T00:00:00Z\",\"n\":2}\n\n")
		st := framesStep(1)
		st.Ignore = []string{"$.n"}
		require.Empty(t, diffOf(t, scenarioOf(st), target(a.URL), target(b.URL)))
	})
}

func TestReadFramesParsing(t *testing.T) {
	in := "event: x\r\ndata: line1\r\ndata: line2\r\n\r\nretry: 10\n\nno-colon\n\ndata: unterminated"
	v, err := readFrames(bytes.NewBufferString(in), 10, normalize.New(), nil)
	require.NoError(t, err)
	require.Equal(t, []any{
		map[string]any{"fields": "event: ,data: ,data: ", "event": "x", "data": "line1\nline2"},
		map[string]any{"fields": "retry: ", "retry": "10"},
		map[string]any{"fields": "no-colon", "no-colon": ""},
	}, v)
}

// A stream with no initial frame gets its frame from a request sent once it's
// open, and that request's status is compared too.
func TestTriggerFiresAfterTheStreamOpens(t *testing.T) {
	newServer := func(triggerStatus int) *httptest.Server {
		var mu sync.Mutex
		var subs []chan string
		return serve(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				mu.Lock()
				for _, s := range subs {
					s <- "data: fired\n\n"
				}
				mu.Unlock()
				w.WriteHeader(triggerStatus)
				return
			}
			ch := make(chan string, 1)
			mu.Lock()
			subs = append(subs, ch)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			rc := http.NewResponseController(w)
			_ = rc.Flush()
			select {
			case f := <-ch:
				_, _ = io.WriteString(w, f)
				_ = rc.Flush()
			case <-r.Context().Done():
				return
			}
			<-r.Context().Done()
		})
	}
	st := framesStep(1)
	st.FramesTimeout = 3 * time.Second
	st.Trigger = &Trigger{As: "alice", Request: Request{Method: http.MethodPost, Path: "/write"}}

	a := newServer(http.StatusCreated)
	tr, err := Run(context.Background(), scenarioOf(st), target(a.URL))
	require.NoError(t, err)
	require.Equal(t, []any{map[string]any{"fields": "data: ", "data": "fired"}}, tr.Steps[0].Body)
	require.Equal(t, http.StatusCreated, tr.Steps[0].TriggerStatus)

	diffs := diffOf(t, scenarioOf(st), target(newServer(http.StatusCreated).URL), target(newServer(http.StatusForbidden).URL))
	require.Len(t, diffs, 1)
	require.Equal(t, "trigger-status", diffs[0].Field)
}

func TestValidateNewStepOptions(t *testing.T) {
	get := Request{Method: http.MethodGet, Path: "/x"}
	raw, b64, badB64 := "x", "eA==", "!!!"
	for name, st := range map[string]Step{
		"frames without stream":     {Request: get, Frames: 1},
		"negative frames":           {Request: get, Stream: true, Frames: -1},
		"timeout without frames":    {Request: get, Stream: true, FramesTimeout: time.Second},
		"timeout over the limit":    {Request: get, Stream: true, Frames: 1, FramesTimeout: time.Minute},
		"trigger without frames":    {Request: get, Stream: true, Trigger: &Trigger{Request: get}},
		"trigger without a request": {Request: get, Stream: true, Frames: 1, Trigger: &Trigger{}},
		"binary and stream":         {Request: get, Binary: true, Stream: true},
		"binary and capture":        {Request: get, Binary: true, Capture: map[string]string{"a": "$.a"}},
		"two bodies":                {Request: Request{Method: "PUT", Path: "/x", Raw: &raw, Base64: &b64}},
		"bad base64":                {Request: Request{Method: "PUT", Path: "/x", Base64: &badB64}},
		"empty fill":                {Request: Request{Method: "PUT", Path: "/x", Fill: &Fill{Size: 1}}},
		"oversized fill":            {Request: Request{Method: "PUT", Path: "/x", Fill: &Fill{Text: "x", Size: maxFill + 1}}},
		"chunked with no body":      {Request: Request{Method: "PUT", Path: "/x", Chunked: true}},
		"mask without files":        {Request: get, FilesMask: []string{"x"}},
		"normalize without files":   {Request: get, FilesNormalizeContent: true},
		"bad mask":                  {Request: get, Files: []string{"alice"}, FilesMask: []string{"("}},
	} {
		st.Name = "s"
		require.Error(t, scenarioOf(st).validate(), name)
	}
}

func writeTree(t *testing.T, root, tenant string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, "tenants", tenant, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
}

func fileScenario(mutate ...func(*Step)) Scenario {
	st := Step{Name: "s", Files: []string{"alice"}, Request: Request{Method: http.MethodGet, Path: "/x"}}
	for _, m := range mutate {
		m(&st)
	}
	return scenarioOf(st)
}

func fileTargets(t *testing.T) (a, b Target, rootA, rootB string) {
	t.Helper()
	srv := serve(t, func(http.ResponseWriter, *http.Request) {})
	rootA, rootB = t.TempDir(), t.TempDir()
	a, b = target(srv.URL), target(srv.URL)
	a.Files, b.Files = DirFiles(rootA), DirFiles(rootB)
	return a, b, rootA, rootB
}

const aliceTree = "A11CE000-0000-4000-8000-000000000001"

func TestFilesAreCompared(t *testing.T) {
	t.Run("same tree", func(t *testing.T) {
		a, b, ra, rb := fileTargets(t)
		tree := map[string]string{"raw/a.md": "hello", "wiki/b/c.txt": "x"}
		writeTree(t, ra, aliceTree, tree)
		writeTree(t, rb, aliceTree, tree)
		require.Empty(t, diffOf(t, fileScenario(), a, b))
	})
	t.Run("both empty", func(t *testing.T) {
		a, b, _, _ := fileTargets(t)
		require.Empty(t, diffOf(t, fileScenario(), a, b))
	})
	t.Run("content differs at the same size", func(t *testing.T) {
		a, b, ra, rb := fileTargets(t)
		writeTree(t, ra, aliceTree, map[string]string{"raw/a.md": "hello"})
		writeTree(t, rb, aliceTree, map[string]string{"raw/a.md": "jello"})
		diffs := diffOf(t, fileScenario(), a, b)
		require.Len(t, diffs, 1)
		require.Equal(t, "files:alice$[1].sha256", diffs[0].Field)
	})
	t.Run("a missing file", func(t *testing.T) {
		a, b, ra, rb := fileTargets(t)
		writeTree(t, ra, aliceTree, map[string]string{"raw/a.md": "x", "raw/b.md": "y"})
		writeTree(t, rb, aliceTree, map[string]string{"raw/a.md": "x"})
		require.Equal(t, "files:alice$.length", diffOf(t, fileScenario(), a, b)[0].Field)
	})
	t.Run("an empty directory counts", func(t *testing.T) {
		a, b, ra, rb := fileTargets(t)
		writeTree(t, ra, aliceTree, map[string]string{"raw/a.md": "x"})
		writeTree(t, rb, aliceTree, map[string]string{"raw/a.md": "x"})
		require.NoError(t, os.MkdirAll(filepath.Join(rb, "tenants", aliceTree, "wiki"), 0o755))
		require.NotEmpty(t, diffOf(t, fileScenario(), a, b))
	})
	t.Run("the mode counts", func(t *testing.T) {
		a, b, ra, rb := fileTargets(t)
		writeTree(t, ra, aliceTree, map[string]string{"a.md": "x"})
		writeTree(t, rb, aliceTree, map[string]string{"a.md": "x"})
		require.NoError(t, os.Chmod(filepath.Join(rb, "tenants", aliceTree, "a.md"), 0o600))
		diffs := diffOf(t, fileScenario(), a, b)
		require.Len(t, diffs, 1)
		require.Equal(t, "files:alice$[0].mode", diffs[0].Field)
	})
	t.Run("another tenant is out of scope", func(t *testing.T) {
		a, b, ra, _ := fileTargets(t)
		writeTree(t, ra, "B0B00000-0000-4000-8000-000000000002", map[string]string{"a.md": "x"})
		require.Empty(t, diffOf(t, fileScenario(), a, b))
	})
	t.Run("server-generated UUIDs in names, whatever their sort order", func(t *testing.T) {
		a, b, ra, rb := fileTargets(t)
		writeTree(t, ra, aliceTree, map[string]string{
			"raw/AAAAAAAA-1111-4111-8111-111111111111.md": "first",
			"raw/FFFFFFFF-9999-4999-8999-999999999999.md": "second",
		})
		writeTree(t, rb, aliceTree, map[string]string{
			"raw/BBBBBBBB-AAAA-4AAA-8AAA-AAAAAAAAAAAA.md": "second",
			"raw/CCCCCCCC-2222-4222-8222-222222222222.md": "first",
		})
		require.Empty(t, diffOf(t, fileScenario(), a, b))
	})
	t.Run("a UUID in a name matches the same UUID in the response", func(t *testing.T) {
		rootA, rootB := t.TempDir(), t.TempDir()
		respond := func(id string) *httptest.Server {
			return serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
			})
		}
		const idA, idB = "AAAAAAAA-1111-4111-8111-111111111111", "BBBBBBBB-2222-4222-8222-222222222222"
		a, b := target(respond(idA).URL), target(respond(idB).URL)
		a.Files, b.Files = DirFiles(rootA), DirFiles(rootB)
		writeTree(t, rootA, aliceTree, map[string]string{"raw/" + idA + ".md": "x"})
		writeTree(t, rootB, aliceTree, map[string]string{"raw/" + idB + ".md": "x"})
		tr, err := Run(context.Background(), fileScenario(), a)
		require.NoError(t, err)
		files := tr.Steps[0].Files["alice"].([]any)
		require.Equal(t, "raw/<uuid#1:upper>.md", files[len(files)-1].(map[string]any)["path"])
		require.Empty(t, diffOf(t, fileScenario(), a, b))
	})
	t.Run("masks and normalized content", func(t *testing.T) {
		mask := func(s *Step) { s.FilesMask = []string{`_deleted_\d+`} }
		norm := func(s *Step) { s.FilesNormalizeContent = true }

		a, b, ra, rb := fileTargets(t)
		writeTree(t, ra, aliceTree, map[string]string{"raw/_deleted_1700000000_x/a": "same"})
		writeTree(t, rb, aliceTree, map[string]string{"raw/_deleted_1700000001_x/a": "same"})
		require.NotEmpty(t, diffOf(t, fileScenario(), a, b))
		require.Empty(t, diffOf(t, fileScenario(mask), a, b))

		a, b, ra, rb = fileTargets(t)
		writeTree(t, ra, aliceTree, map[string]string{"a.md": "id: AAAAAAAA-1111-4111-8111-111111111111\nat: 2026-01-01T00:00:00Z\n"})
		writeTree(t, rb, aliceTree, map[string]string{"a.md": "id: BBBBBBBB-2222-4222-8222-222222222222\nat: 2026-06-06T12:00:00Z\n"})
		require.NotEmpty(t, diffOf(t, fileScenario(), a, b), "raw bytes differ")
		require.Empty(t, diffOf(t, fileScenario(norm), a, b))

		writeTree(t, rb, aliceTree, map[string]string{"a.md": "id: BBBBBBBB-2222-4222-8222-222222222222\nat: 2026-06-06T12:00:00.5Z\n"})
		require.NotEmpty(t, diffOf(t, fileScenario(norm), a, b), "the timestamp's form still counts")
	})
	t.Run("a UUID entry and a captured var name a team vault's tree", func(t *testing.T) {
		const vault = "7EA53000-0000-4000-8000-000000000001"
		srv := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintf(w, `{"id":%q}`, vault) })
		root := t.TempDir()
		writeTree(t, root, vault, map[string]string{"raw/a": "x"})
		tg := target(srv.URL)
		tg.Files = DirFiles(root)
		sc := scenarioOf(Step{Name: "s", Capture: map[string]string{"v": "$.id"}, Files: []string{"{{v}}", vault}, Request: Request{Method: http.MethodGet, Path: "/x"}})
		tr, err := Run(context.Background(), sc, tg)
		require.NoError(t, err)
		require.NotEmpty(t, tr.Steps[0].Files["{{v}}"])
		require.NotEmpty(t, tr.Steps[0].Files[vault])

		bad := fileScenario(func(s *Step) { s.Files = []string{"nobody"} })
		_, err = Run(context.Background(), bad, tg)
		require.ErrorContains(t, err, "neither a fixture user nor a UUID")
	})
}

func TestFilesNeedAReaderExceptInSmoke(t *testing.T) {
	srv := serve(t, func(http.ResponseWriter, *http.Request) {})
	_, err := Run(context.Background(), fileScenario(), target(srv.URL))
	require.ErrorContains(t, err, "--swift-files")

	smoke := Target{Name: "staging", BaseURL: srv.URL, Token: "t"}
	tr, err := Run(context.Background(), fileScenario(), smoke)
	require.NoError(t, err)
	require.Nil(t, tr.Steps[0].Files)
}

func TestNewFileReader(t *testing.T) {
	for _, ok := range []string{"dir:/x", "docker:c:/x"} {
		_, err := NewFileReader(ok)
		require.NoError(t, err, ok)
	}
	for _, bad := range []string{"", "/x", "dir:", "docker:", "docker:c", "docker::/x", "docker:c:"} {
		_, err := NewFileReader(bad)
		require.Error(t, err, bad)
	}
}

// `docker cp` streams the tree with its own directory as the first element.
func TestReadTarDropsTheTreeDirectory(t *testing.T) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		require.NoError(t, w.WriteHeader(h))
		_, err := io.WriteString(w, body)
		require.NoError(t, err)
	}
	add(&tar.Header{Name: aliceTree + "/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: aliceTree + "/raw/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: aliceTree + "/raw/a.md", Typeflag: tar.TypeReg, Mode: 0o644}, "hello")
	add(&tar.Header{Name: aliceTree + "/raw/l", Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: "a.md"}, "")
	require.NoError(t, w.Close())

	got, err := readTar(&buf)
	require.NoError(t, err)
	require.Equal(t, []FileEntry{
		{Path: "raw", Type: "dir", Mode: 0o755},
		{Path: "raw/a.md", Type: "file", Mode: 0o644, Content: []byte("hello")},
		{Path: "raw/l", Type: "symlink", Mode: 0o777, Link: "a.md"},
	}, got)
}

func TestCompareTreeIsStableForIdenticalNames(t *testing.T) {
	entries := func(first, second string) []FileEntry {
		return []FileEntry{
			{Path: first + ".md", Type: "file", Mode: fs.FileMode(0o644), Content: []byte("b")},
			{Path: second + ".md", Type: "file", Mode: fs.FileMode(0o644), Content: []byte("a")},
		}
	}
	const x, y = "AAAAAAAA-1111-4111-8111-111111111111", "BBBBBBBB-2222-4222-8222-222222222222"
	var none []*regexp.Regexp
	require.Equal(t,
		compareTree(entries(x, y), none, false, normalize.New()),
		compareTree(entries(y, x), none, false, normalize.New()),
	)
}
