package connectedapps_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the test's working directory to the directory that
// holds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// TestNoAdminOrTelemetryReadsConnectedAppAccounts is a static privacy guard:
// connected-app tokens and the data apps return are for the user's own turn
// only. Admin screens, product analytics, telemetry and the LLM transport
// layer must never read the account table or the store that wraps it. If this
// fails, remove the reference; do not narrow the test.
func TestNoAdminOrTelemetryReadsConnectedAppAccounts(t *testing.T) {
	root := repoRoot(t)
	dirs := []string{
		"internal/modules/admin",
		"internal/posthog",
		"internal/telemetry",
		"internal/llm",
	}
	banned := []string{"connected_app_accounts", "connectedapps.Store"}

	for _, d := range dirs {
		full := filepath.Join(root, filepath.FromSlash(d))
		info, err := os.Stat(full)
		if err != nil || !info.IsDir() {
			t.Errorf("guarded directory %s does not exist: the guard would pass silently; fix the path", d)
			continue
		}
		scanned := 0
		err = filepath.WalkDir(full, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			scanned++
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, word := range banned {
				if strings.Contains(string(b), word) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s references %q", rel, word)
				}
			}
			return nil
		})
		if err != nil {
			t.Errorf("walk %s: %v", d, err)
		}
		if scanned == 0 {
			t.Errorf("guarded directory %s holds no .go files", d)
		}
	}
}
