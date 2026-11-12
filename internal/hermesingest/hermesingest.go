// Package hermesingest is URLSessionHermesIngestionTransport: the call
// MultimodalIngestionWorker's Hermes processor makes to hand a remote source to
// Hermes' dedicated ingestion endpoint. The worker itself still runs on Swift
// (jobs.md); this is the transport it will use when it moves.
package hermesingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/hermesresolve"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// Timeout is the request's timeoutInterval.
const Timeout = 180 * time.Second

// Resolver is HermesEndpointResolver.
type Resolver interface {
	Resolve(ctx context.Context, tenant uuid.UUID) (hermesresolve.Resolution, error)
}

// Transport is URLSessionHermesIngestionTransport.
type Transport struct {
	DefaultBaseURL    *url.URL
	DefaultAuthHeader *string
	Resolver          Resolver // optional
	HTTP              *http.Client
	Log               *slog.Logger
}

type requestBody struct {
	SourceURL    string  `json:"source_url"`
	ContentType  string  `json:"content_type"`
	Instructions *string `json:"instructions,omitempty"`
}

// Ingest is ingest(tenantID:sourceURL:contentType:instructions:): POST
// <base>/v1/ingestions with the tenant as X-Hermes-Session-Key. A non-2xx answer
// is a 502 "Hermes ingestion endpoint failed".
func (t *Transport) Ingest(ctx context.Context, tenant uuid.UUID, sourceURL *url.URL, contentType string, instructions *string) ([]byte, error) {
	base, auth, err := t.endpoint(ctx, tenant)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(requestBody{SourceURL: sourceURL.String(), ContentType: contentType, Instructions: instructions})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("v1", "ingestions").String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hermes-Session-Key", ids.Upper(tenant))
	if auth != nil && *auth != "" {
		req.Header.Set("Authorization", *auth)
	}
	client := t.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.log().ErrorContext(ctx, fmt.Sprintf("hermes ingestion endpoint failed status=%d", resp.StatusCode))
		return nil, wire.NewError(http.StatusBadGateway, "Hermes ingestion endpoint failed")
	}
	return data, nil
}

func (t *Transport) endpoint(ctx context.Context, tenant uuid.UUID) (*url.URL, *string, error) {
	if t.Resolver == nil {
		return t.DefaultBaseURL, t.DefaultAuthHeader, nil
	}
	r, err := t.Resolver.Resolve(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}
	if r.IsUserOverride {
		return r.BaseURL, r.AuthHeader, nil
	}
	return r.BaseURL, t.DefaultAuthHeader, nil
}

func (t *Transport) log() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default().With("label", "lv.ingestion.hermes")
}
