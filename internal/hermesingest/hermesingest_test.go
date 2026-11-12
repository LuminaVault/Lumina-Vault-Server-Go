package hermesingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func transport(t *testing.T, srv *httptest.Server) *Transport {
	t.Helper()
	base, err := url.Parse(srv.URL)
	require.NoError(t, err)
	auth := "Bearer managed-key"
	return &Transport{DefaultBaseURL: base, DefaultAuthHeader: &auth, HTTP: srv.Client(), Log: slog.New(slog.DiscardHandler)}
}

// HermesIngestionTransportTests: posts remote source to dedicated Hermes ingestion endpoint.
func TestPostsRemoteSourceToDedicatedHermesIngestionEndpoint(t *testing.T) {
	var captured *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"object":"hermes.ingestion.result","analysis":{}}`))
	}))
	defer srv.Close()
	tenant := uuid.New()
	source, _ := url.Parse("https://vault.test/v1/ingestion-sources/token")
	instructions := "Include page references."

	_, err := transport(t, srv).Ingest(context.Background(), tenant, source, "application/pdf", &instructions)
	require.NoError(t, err)

	require.NotNil(t, captured)
	assert.Equal(t, "/v1/ingestions", captured.URL.Path)
	assert.Equal(t, http.MethodPost, captured.Method)
	assert.Equal(t, "Bearer managed-key", captured.Header.Get("Authorization"))
	assert.Equal(t, ids.Upper(tenant), captured.Header.Get("X-Hermes-Session-Key"))
	var decoded map[string]string
	require.NoError(t, json.Unmarshal(body, &decoded))
	assert.Equal(t, "https://vault.test/v1/ingestion-sources/token", decoded["source_url"])
	assert.Equal(t, "application/pdf", decoded["content_type"])
	assert.Equal(t, "Include page references.", decoded["instructions"])
}

// HermesIngestionTransportTests: non success response is surfaced as bad gateway.
func TestNonSuccessResponseIsSurfacedAsBadGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable"))
	}))
	defer srv.Close()
	source, _ := url.Parse("https://vault.test/source")

	_, err := transport(t, srv).Ingest(context.Background(), uuid.New(), source, "image/jpeg", nil)
	var we *wire.Error
	require.True(t, errors.As(err, &we), "want a wire.Error, got %v", err)
	assert.Equal(t, http.StatusBadGateway, we.Status)
}
