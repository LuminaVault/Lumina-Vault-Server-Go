package vaultfiles

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeUTF8(t *testing.T) {
	got, ok := DecodeUTF8([]byte("\xef\xbb\xbf\xef\xbb\xbfx"))
	require.True(t, ok)
	require.Equal(t, "\ufeffx", got)
	_, ok = DecodeUTF8([]byte{0xff, 0xff})
	require.False(t, ok)
}
