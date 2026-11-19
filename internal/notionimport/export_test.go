package notionimport

import (
	"bytes"
	"lumina/internal/notionimport/notiontest"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	idRoadmap  = notiontest.IDRoadmap
	idQ4       = notiontest.IDQ4
	idTasks    = notiontest.IDTasks
	idRowLogin = notiontest.IDRowLogin
	idRowDocs  = notiontest.IDRowDocs
	idUntitled = notiontest.IDUntitled
	idUntitle2 = notiontest.IDUntitled2
	idGone     = notiontest.IDGone
	idChild    = notiontest.IDChild
)

type zipFile = notiontest.File

var pngBytes = notiontest.PNG

func buildZip(_ *testing.T, files ...zipFile) []byte { return notiontest.Zip(files...) }

func text(name, body string) zipFile { return notiontest.Text(name, body) }

func syntheticExport(_ *testing.T) []byte { return notiontest.Export() }

func unpack(t *testing.T, data []byte) *Archive {
	t.Helper()
	a, err := Unpack(bytes.NewReader(data), int64(len(data)), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(a.Close)
	return a
}
