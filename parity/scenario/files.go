package scenario

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"lumina/internal/ids"
	"lumina/parity/normalize"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// FileEntry is one thing in a vault tree. Path is relative to the tree's root
// and always uses "/". Ownership and modification times aren't compared.
type FileEntry struct {
	Path    string
	Type    string // "file", "dir" or "symlink"
	Mode    fs.FileMode
	Content []byte
	Link    string
}

// FileReader lists the tree at rel, a slash path under the server's vault root.
// A tree that doesn't exist has no entries; that isn't an error.
type FileReader func(ctx context.Context, rel string) ([]FileEntry, error)

// NewFileReader parses --swift-files and --go-files:
//
//	docker:<container>:<vault root>  read inside a container with `docker cp`
//	dir:<vault root>                 read a directory on this machine
//
// `docker cp` works with every image (the Go image is distroless, with no shell
// or tar) and sees files a runner user couldn't read through a bind mount.
func NewFileReader(spec string) (FileReader, error) {
	if root, ok := strings.CutPrefix(spec, "dir:"); ok && root != "" {
		return DirFiles(root), nil
	}
	if rest, ok := strings.CutPrefix(spec, "docker:"); ok {
		if container, root, ok := strings.Cut(rest, ":"); ok && container != "" && root != "" {
			return DockerFiles(container, root), nil
		}
	}
	return nil, fmt.Errorf("bad files spec %q: want docker:<container>:<root> or dir:<root>", spec)
}

// DirFiles reads trees from a directory on this machine.
func DirFiles(root string) FileReader {
	return func(_ context.Context, rel string) ([]FileEntry, error) {
		base := filepath.Join(root, filepath.FromSlash(rel))
		var out []FileEntry
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == base {
				return nil
			}
			name, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			e := FileEntry{Path: filepath.ToSlash(name), Mode: info.Mode().Perm()}
			switch {
			case d.IsDir():
				e.Type = "dir"
			case info.Mode()&fs.ModeSymlink != 0:
				e.Type = "symlink"
				if e.Link, err = os.Readlink(p); err != nil {
					return err
				}
			default:
				e.Type = "file"
				if e.Content, err = os.ReadFile(p); err != nil { //nolint:gosec // operator's own test directory
					return err
				}
			}
			out = append(out, e)
			return nil
		})
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return out, err
	}
}

// DockerFiles reads trees from inside a container.
func DockerFiles(container, root string) FileReader {
	return func(ctx context.Context, rel string) ([]FileEntry, error) {
		src := container + ":" + path.Join(root, rel)
		cmd := exec.CommandContext(ctx, "docker", "cp", src, "-") //nolint:gosec // operator-supplied container and fixture paths
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			msg := stderr.String()
			// A missing path, in each spelling the Docker CLI has used. A missing
			// container is "No such container: x" and stays an error.
			if strings.Contains(msg, "Could not find the file") || strings.Contains(msg, "No such container:path") {
				return nil, nil
			}
			return nil, fmt.Errorf("docker cp %s: %w: %s", src, err, msg)
		}
		return readTar(&stdout)
	}
}

// readTar turns `docker cp`'s stream into entries. Its names start with the
// tree's own directory, which is dropped.
func readTar(r io.Reader) ([]FileEntry, error) {
	tr := tar.NewReader(r)
	var out []FileEntry
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		_, name, _ := strings.Cut(strings.TrimSuffix(h.Name, "/"), "/")
		if name == "" {
			continue
		}
		e := FileEntry{Path: name, Mode: h.FileInfo().Mode().Perm()}
		switch h.Typeflag {
		case tar.TypeDir:
			e.Type = "dir"
		case tar.TypeSymlink:
			e.Type, e.Link = "symlink", h.Linkname
		default:
			e.Type = "file"
			if e.Content, err = io.ReadAll(tr); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
}

// snapshotFiles returns the step's trees, keyed by the name used in `files`.
func snapshotFiles(ctx context.Context, st Step, t Target, norm *normalize.Normalizer, vars map[string]string) (map[string]any, error) {
	if len(st.Files) == 0 {
		return nil, nil
	}
	if t.Files == nil {
		if t.Token != "" {
			return nil, nil // smoke: a deployed server's volume isn't reachable
		}
		return nil, errors.New("a step with files needs --swift-files and --go-files")
	}
	masks := make([]*regexp.Regexp, len(st.FilesMask))
	for i, m := range st.FilesMask {
		var err error
		if masks[i], err = regexp.Compile(m); err != nil {
			return nil, err
		}
	}
	out := make(map[string]any, len(st.Files))
	for _, name := range st.Files {
		tenant, err := resolveTree(name, t, vars)
		if err != nil {
			return nil, err
		}
		entries, err := t.Files(ctx, "tenants/"+ids.Upper(tenant))
		if err != nil {
			return nil, fmt.Errorf("files %s: %w", name, err)
		}
		out[name] = compareTree(entries, masks, st.FilesNormalizeContent, norm)
	}
	return out, nil
}

// resolveTree reads a `files` entry as a fixture user, or as a UUID or a var
// holding one (a team vault has its own tree).
func resolveTree(name string, t Target, vars map[string]string) (uuid.UUID, error) {
	if id, ok := t.Users[name]; ok {
		return id, nil
	}
	id, err := ids.Parse(substitute(name, vars))
	if err != nil {
		return uuid.Nil, fmt.Errorf("files: %q is neither a fixture user nor a UUID", name)
	}
	return id, nil
}

// compareTree normalizes entries into a stable list. Each server names files
// with its own UUIDs, so entries are ordered first with every UUID masked; only
// then are UUIDs numbered, in the shared Normalizer, so that a file named after
// a row's id gets the row's placeholder.
func compareTree(entries []FileEntry, masks []*regexp.Regexp, normalizeContent bool, norm *normalize.Normalizer) []any {
	type keyed struct {
		key     string
		path    string
		content string
		entry   FileEntry
	}
	ks := make([]keyed, len(entries))
	for i, e := range entries {
		k := keyed{path: applyMasks(e.Path, masks), content: string(e.Content), entry: e}
		if normalizeContent {
			k.content = applyMasks(k.content, masks)
		}
		order := normalize.New() // fresh, so every UUID reads the same here
		k.key = maskNumbers(order.UUIDs(k.path))
		if normalizeContent {
			k.key += "\x00" + digest(maskNumbers(normalize.New().Text(k.content)))
		} else {
			k.key += "\x00" + digest(k.content)
		}
		ks[i] = k
	}
	sort.SliceStable(ks, func(i, j int) bool { return ks[i].key < ks[j].key })

	out := make([]any, 0, len(ks))
	for _, k := range ks {
		v := map[string]any{
			"path": norm.UUIDs(k.path),
			"type": k.entry.Type,
			"mode": fmt.Sprintf("%04o", uint32(k.entry.Mode)),
		}
		switch k.entry.Type {
		case "file":
			content := k.content
			if normalizeContent {
				content = norm.Text(content)
			}
			v["size"] = len(content)
			v["sha256"] = digest(content)
		case "symlink":
			v["link"] = norm.UUIDs(k.entry.Link)
		}
		out = append(out, v)
	}
	return out
}

func applyMasks(s string, masks []*regexp.Regexp) string {
	for _, m := range masks {
		s = m.ReplaceAllLiteralString(s, "<mask>")
	}
	return s
}

var uuidNumberRe = regexp.MustCompile(`<uuid#\d+:`)

// maskNumbers drops the numbering from placeholders, for sort keys only.
func maskNumbers(s string) string { return uuidNumberRe.ReplaceAllString(s, "<uuid:") }

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
