package vault

import (
	"encoding/json"
	"lumina/internal/ids"
	"lumina/internal/vaultfiles"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CaptureFilingServiceTests asks VaultFileDTO.fromRow for the suggestion. It shows only
// while the file is unfiled.
func TestDTOShowsASuggestedSpaceOnlyWhileUnfiled(t *testing.T) {
	suggested := uuid.New()
	file := &vaultfiles.VaultFile{
		ID: uuid.New(), Path: "inbox/standup.md", ContentType: "text/markdown",
		Metadata: &vaultfiles.Metadata{SuggestedSpaceID: &suggested},
	}
	dto := toDTO(file)
	require.NotNil(t, dto.Metadata)
	require.NotNil(t, dto.Metadata.SuggestedSpaceID)
	assert.Equal(t, suggested, dto.Metadata.SuggestedSpaceID.UUID)
	data, err := json.Marshal(dto)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"suggestedSpaceId":"`+ids.Upper(suggested)+`"`)

	space := uuid.New()
	file.SpaceID = &space
	dto = toDTO(file)
	require.NotNil(t, dto.Metadata)
	assert.Nil(t, dto.Metadata.SuggestedSpaceID)
}
