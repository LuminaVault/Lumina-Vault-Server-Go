package ingestion

import (
	"context"
	"encoding/json"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/wire"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// Result is HermesIngestionResult.
type Result struct {
	Title       string
	Markdown    string
	Summary     string
	Tags        []string
	Credibility Credibility
}

// Credibility is IngestionCredibilityRecord, fields in Swift's declaration order so
// the jsonb column holds what Fluent writes. A nil score is omitted, as Swift's
// synthesized encodeIfPresent does.
type Credibility struct {
	Score      *int     `json:"score,omitempty"`
	Confidence float64  `json:"confidence"`
	Signals    []string `json:"signals"`
	Rationale  string   `json:"rationale"`
	Version    string   `json:"version"`
}

// Processor is HermesMultimodalProcessor.process.
type Processor interface {
	Process(ctx context.Context, tenant uuid.UUID, source, contentType string) (Result, error)
}

// ChatTransport is HermesChatTransport.chatCompletions.
type ChatTransport interface {
	ChatCompletions(ctx context.Context, payload []byte, sessionKey, sessionID string) (llm.Metadata, error)
}

// IngestTransport is HermesIngestionTransport.ingest.
type IngestTransport interface {
	Ingest(ctx context.Context, tenant uuid.UUID, sourceURL *url.URL, contentType string, instructions *string) ([]byte, error)
}

// HermesProcessor is HermesMultimodalProcessor: an http(s) source goes to the
// Hermes ingestion endpoint when one is wired, anything else (a local vault path)
// to a JSON-mode chat completion over the routed transport.
type HermesProcessor struct {
	Chat   ChatTransport
	Ingest IngestTransport // optional
	Model  string
}

const ingestInstructions = "Include OCR, transcript, page or timestamp references, and important facts in markdown when applicable."

var errNoIngestionResult = wire.NewError(http.StatusBadGateway, "Hermes returned no ingestion result")

type resultPayload struct {
	Title       string   `json:"title"`
	Markdown    string   `json:"markdown"`
	Summary     string   `json:"summary"`
	Tags        []string `json:"tags"`
	Credibility struct {
		Score      *int     `json:"score"`
		Confidence float64  `json:"confidence"`
		Signals    []string `json:"signals"`
		Rationale  string   `json:"rationale"`
		Version    string   `json:"version"`
	} `json:"credibility"`
}

// UnmarshalJSON is the synthesized Decodable: every key is required and an
// explicit null is a type mismatch, except credibility.score.
func (p *resultPayload) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if err := requireKeys(raw, "title", "markdown", "summary", "tags", "credibility"); err != nil {
		return err
	}
	var cred map[string]json.RawMessage
	if err := json.Unmarshal(raw["credibility"], &cred); err != nil {
		return err
	}
	if cred == nil {
		return errKeyNotFound("credibility")
	}
	if err := requireKeys(cred, "confidence", "signals", "rationale", "version"); err != nil {
		return err
	}
	type plain resultPayload
	var out plain
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*p = resultPayload(out)
	return nil
}

func requireKeys(raw map[string]json.RawMessage, keys ...string) error {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			return errKeyNotFound(k)
		}
		if string(v) == "null" {
			return &decodeError{"valueNotFound(" + k + ")"}
		}
	}
	return nil
}

type decodeError struct{ s string }

func (e *decodeError) Error() string { return e.s }

func errKeyNotFound(key string) error { return &decodeError{"keyNotFound(" + key + ")"} }

// Process is process(tenantID:source:contentType:).
func (p *HermesProcessor) Process(ctx context.Context, tenant uuid.UUID, source, contentType string) (Result, error) {
	if u, err := url.Parse(source); err == nil && p.Ingest != nil {
		if scheme := strings.ToLower(u.Scheme); scheme == "http" || scheme == "https" {
			instructions := ingestInstructions
			raw, err := p.Ingest.Ingest(ctx, tenant, u, contentType, &instructions)
			if err != nil {
				return Result{}, err
			}
			var envelope struct {
				Analysis *resultPayload `json:"analysis"`
			}
			if err := json.Unmarshal(raw, &envelope); err != nil {
				return Result{}, err
			}
			if envelope.Analysis == nil {
				return Result{}, errKeyNotFound("analysis")
			}
			return toResult(*envelope.Analysis), nil
		}
	}
	prompt := "Analyze the source below using your available vision, document, audio, video, and web tools.\n" +
		"The source is already in the user's LuminaVault and must remain the source of truth.\n" +
		"Return JSON only with keys: title, markdown, summary, tags, credibility.\n" +
		"credibility has score (0...100 or null for personal media), confidence (0...1), signals, rationale, version.\n" +
		"Include OCR, transcript, page/time references, and important facts in markdown when applicable.\n" +
		"Source: " + source + "\n" +
		"Content-Type: " + contentType
	payload, err := wire.MarshalFoundation(struct {
		Model          string            `json:"model"`
		Messages       []chatMessage     `json:"messages"`
		ResponseFormat map[string]string `json:"response_format"`
		Stream         bool              `json:"stream"`
	}{p.Model, []chatMessage{{Role: "user", Content: prompt}}, map[string]string{"type": "json_object"}, false})
	if err != nil {
		return Result{}, err
	}
	meta, err := p.Chat.ChatCompletions(ctx, payload, ids.Upper(tenant), "")
	if err != nil {
		return Result{}, err
	}
	var envelope struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(meta.Data, &envelope); err != nil {
		return Result{}, err
	}
	if len(envelope.Choices) == 0 {
		return Result{}, errNoIngestionResult
	}
	data, ok := jsonData(envelope.Choices[0].Message.Content)
	if !ok {
		return Result{}, errNoIngestionResult
	}
	var result resultPayload
	if err := json.Unmarshal(data, &result); err != nil {
		return Result{}, err
	}
	return toResult(result), nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// toResult clamps the score to 0...100 and the confidence to 0...1.
func toResult(r resultPayload) Result {
	c := r.Credibility
	var score *int
	if c.Score != nil {
		s := min(100, max(0, *c.Score))
		score = &s
	}
	signals := c.Signals
	if signals == nil {
		signals = []string{}
	}
	tags := r.Tags
	if tags == nil {
		tags = []string{}
	}
	return Result{
		Title: r.Title, Markdown: r.Markdown, Summary: r.Summary, Tags: tags,
		Credibility: Credibility{
			Score: score, Confidence: min(1, max(0, c.Confidence)), Signals: signals,
			Rationale: c.Rationale, Version: c.Version,
		},
	}
}

// jsonData strips code fences and keeps the first "{" through the last "}".
func jsonData(value string) ([]byte, bool) {
	text := strings.TrimSpace(value)
	text = strings.ReplaceAll(text, "```json", "")
	text = strings.ReplaceAll(text, "```", "")
	start, end := strings.IndexByte(text, '{'), strings.LastIndexByte(text, '}')
	if start < 0 || end < 0 || end < start {
		return nil, false
	}
	return []byte(text[start : end+1]), true
}
