package marketplace

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// CaptureHookDispatcherTests "reading-time is a captureHook entry in the catalog":
// its slug and binding are in internal/capture, and its listing here.
func TestReadingTimeCatalogEntry(t *testing.T) {
	for _, e := range firstPartyCatalog {
		if e.slug == "reading-time" {
			require.Equal(t, "capture", e.category)
			require.Empty(t, e.configFields)
			return
		}
	}
	t.Fatal("reading-time is not in the catalog")
}
