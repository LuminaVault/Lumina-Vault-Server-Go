package llm

// Ports Tests/AppTests/Memory/URLSessionHermesChatTransportTests.swift.
//
// Go has no URLSessionHermesChatTransport. In Swift that struct is no longer
// constructed anywhere in Sources (only named in comments); HermesGatewayAdapter
// replaced it on the same wire. Its Go port is HermesGatewayAdapter (hermes.go),
// so the three cases run against that, with the API key turned into the
// Authorization value exactly as stack.go does ("Bearer " + HERMES_API_KEY, or
// nothing when the key is empty).

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatewayFor is URLSessionHermesChatTransport(apiKey:) as stack.go builds it.
func gatewayFor(t *testing.T, stub *wireStub, apiKey string) *HermesGatewayAdapter {
	t.Helper()
	a := &HermesGatewayAdapter{BaseURL: stub.url(t)}
	if apiKey != "" {
		a.DefaultAuthHeader = "Bearer " + apiKey
	}
	return a
}

const auditFPayload = `{"model":"hermes-3","messages":[]}`

func TestAuditFBearerHeaderIsSentWhenAPIKeyIsNonEmpty(t *testing.T) {
	stub := newWireStub(t, 200, `{"choices":[]}`)
	_, err := gatewayFor(t, stub, "k-test").ChatCompletions(context.Background(), []byte(auditFPayload), "tenant-uuid", "conv-123")
	require.NoError(t, err)

	require.Equal(t, 1, stub.count())
	req := stub.last(t)
	assert.Equal(t, "/v1/chat/completions", req.path)
	assert.Equal(t, "Bearer k-test", req.header.Get("Authorization"))
	assert.Equal(t, "tenant-uuid", req.header.Get("X-Hermes-Session-Key"))
	assert.Equal(t, "conv-123", req.header.Get("X-Hermes-Session-Id"))
	assert.Equal(t, "application/json", req.header.Get("Content-Type"))
	assert.Empty(t, req.header.Values("X-Hermes-Profile"))
	assert.JSONEq(t, auditFPayload, string(req.raw))
}

func TestAuditFBearerHeaderIsOmittedWhenAPIKeyIsEmpty(t *testing.T) {
	stub := newWireStub(t, 200, `{"choices":[]}`)
	_, err := gatewayFor(t, stub, "").ChatCompletions(context.Background(), []byte(auditFPayload), "tenant-uuid", "")
	require.NoError(t, err)

	require.Equal(t, 1, stub.count())
	req := stub.last(t)
	assert.Empty(t, req.header.Values("Authorization"))
	assert.Equal(t, "tenant-uuid", req.header.Get("X-Hermes-Session-Key"))
	assert.Empty(t, req.header.Values("X-Hermes-Session-Id"))
}

// Swift throws HTTPError(.badGateway) on any non-2xx. The Go adapter instead
// classifies the status: a 401 is a permanent ProviderError carrying the
// upstream status. Callers render it as a 502 upstream envelope (the routed
// transport fails over first), so the observable contract is "an error, never
// a body", and that is what is asserted.
func TestAuditFNon2xxUpstreamSurfacesAnError(t *testing.T) {
	stub := newWireStub(t, 401, "unauthorized")
	m, err := gatewayFor(t, stub, "k-test").ChatCompletions(context.Background(), []byte(auditFPayload), "tenant-uuid", "")
	require.Error(t, err)
	assert.Nil(t, m.Data)

	var perr *ProviderError
	require.True(t, errors.As(err, &perr))
	assert.Equal(t, KindPermanent, perr.Kind)
	assert.Equal(t, 401, perr.Status)
	assert.Equal(t, HermesGateway, perr.Provider)
	assert.Equal(t, 1, stub.count(), "a permanent failure is not retried")
}
