package ids_test

import (
	"lumina/internal/ids"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUpper(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-55555555abcd")
	require.Equal(t, "11111111-2222-3333-4444-55555555ABCD", ids.Upper(id))
}

func TestUpperMatchesStringsToUpper(t *testing.T) {
	for range 100 {
		id := uuid.New()
		require.Equal(t, strings.ToUpper(id.String()), ids.Upper(id))
	}
}

func TestParseAcceptsBothCases(t *testing.T) {
	lower, err := ids.Parse("aaaaaaaa-0000-4000-8000-000000000001")
	require.NoError(t, err)
	upper, err := ids.Parse("AAAAAAAA-0000-4000-8000-000000000001")
	require.NoError(t, err)
	require.Equal(t, lower, upper)
}
