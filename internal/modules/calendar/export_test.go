package calendar

import (
	"lumina/internal/secretbox"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMasterKey is base64 of 32 bytes of 0x01, the parity env's key.
const TestMasterKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	box, err := secretbox.New(TestMasterKey)
	require.NoError(t, err)
	return box
}
