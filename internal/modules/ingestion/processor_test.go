package ingestion

import (
	"context"
	"encoding/json"
	"lumina/internal/llm"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type stubChat struct {
	payload    []byte
	sessionKey string
	reply      string
}

func (s *stubChat) ChatCompletions(_ context.Context, payload []byte, sessionKey, _ string) (llm.Metadata, error) {
	s.payload, s.sessionKey = payload, sessionKey
	data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": s.reply}}}})
	return llm.Metadata{Data: data}, nil
}

type stubIngest struct {
	source       string
	contentType  string
	instructions *string
	reply        string
}

func (s *stubIngest) Ingest(_ context.Context, _ uuid.UUID, u *url.URL, contentType string, instructions *string) ([]byte, error) {
	s.source, s.contentType, s.instructions = u.String(), contentType, instructions
	return []byte(s.reply), nil
}

var tenant = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")

// A local path goes to a JSON-mode chat completion; fences are stripped and the
// score and confidence clamped.
func TestHermesProcessorChatPath(t *testing.T) {
	chat := &stubChat{reply: "```json\n{\"title\":\"T\",\"markdown\":\"M\",\"summary\":\"S\",\"tags\":[\"a\"]," +
		"\"credibility\":{\"score\":140,\"confidence\":1.5,\"signals\":[],\"rationale\":\"R\",\"version\":\"v\"}}\n```"}
	p := &HermesProcessor{Chat: chat, Ingest: &stubIngest{}, Model: "hermes-default"}
	got, err := p.Process(context.Background(), tenant, "/vault/raw/inbox/a.pdf", "application/pdf")
	require.NoError(t, err)
	require.Equal(t, "T", got.Title)
	require.Equal(t, 100, *got.Credibility.Score)
	require.InDelta(t, 1.0, got.Credibility.Confidence, 0)
	require.Equal(t, "A11CE000-0000-4000-8000-000000000001", chat.sessionKey)

	var body map[string]any
	require.NoError(t, json.Unmarshal(chat.payload, &body))
	require.Equal(t, "hermes-default", body["model"])
	require.Equal(t, false, body["stream"])
	require.Equal(t, map[string]any{"type": "json_object"}, body["response_format"])
	msgs := body["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].(string)
	require.Contains(t, content, "Source: /vault/raw/inbox/a.pdf\nContent-Type: application/pdf")

	chat.reply = "no json here"
	_, err = p.Process(context.Background(), tenant, "/x", "text/plain")
	require.EqualError(t, err, "Hermes returned no ingestion result")

	chat.reply = `{"title":"T","markdown":"M","summary":"S","tags":[],"credibility":{"score":null,"confidence":0.5,"signals":[],"rationale":"R","version":"v"}}`
	got, err = p.Process(context.Background(), tenant, "/x", "text/plain")
	require.NoError(t, err)
	require.Nil(t, got.Credibility.Score)

	chat.reply = `{"title":"T","markdown":"M","summary":"S","credibility":{"confidence":0.5,"signals":[],"rationale":"R","version":"v"}}`
	_, err = p.Process(context.Background(), tenant, "/x", "text/plain")
	require.EqualError(t, err, "keyNotFound(tags)")
}

// An http(s) source goes to the dedicated ingestion endpoint with the
// instructions, and the analysis comes back from the envelope.
func TestHermesProcessorIngestPath(t *testing.T) {
	ingest := &stubIngest{reply: `{"object":"hermes.ingestion.result","analysis":{"title":"T","markdown":"M","summary":"S","tags":["x"],` +
		`"credibility":{"score":-4,"confidence":-1,"signals":["s"],"rationale":"R","version":"v"}}}`}
	p := &HermesProcessor{Chat: &stubChat{}, Ingest: ingest, Model: "m"}
	got, err := p.Process(context.Background(), tenant, "https://api.test/v1/ingestion-sources/tok", "image/png")
	require.NoError(t, err)
	require.Equal(t, "https://api.test/v1/ingestion-sources/tok", ingest.source)
	require.Equal(t, "image/png", ingest.contentType)
	require.Equal(t, ingestInstructions, *ingest.instructions)
	require.Equal(t, 0, *got.Credibility.Score)
	require.InDelta(t, 0.0, got.Credibility.Confidence, 0)
	require.Equal(t, []string{"x"}, got.Tags)

	// Without an ingestion transport a URL goes through chat.
	chat := &stubChat{reply: `{"title":"T","markdown":"M","summary":"S","tags":[],"credibility":{"score":1,"confidence":0.5,"signals":[],"rationale":"R","version":"v"}}`}
	_, err = (&HermesProcessor{Chat: chat, Model: "m"}).Process(context.Background(), tenant, "https://example.com", "text/html")
	require.NoError(t, err)
	require.NotNil(t, chat.payload)
}

func TestRetryDelay(t *testing.T) {
	for attempt, want := range map[int]int{0: 1, 1: 2, 2: 4, 8: 256, 9: 256, 20: 256} {
		require.Equal(t, want, int(retryDelay(attempt).Seconds()), attempt)
	}
}
