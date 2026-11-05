package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Chunk is ChatStreamChunk: one delta. FinishReason is set on the terminal
// chunk. ToolCallID marks a Hermes tool invocation, never transcript content.
type Chunk struct {
	Delta        string
	FinishReason string
	ToolCallID   string
}

// Stream yields chunks in order. A non-nil error ends it. Breaking out of the
// range loop stops the upstream call.
type Stream = iter.Seq2[Chunk, error]

// Metadata is HermesChatTransportMetadata: a body plus lowercased headers.
type Metadata struct {
	Data    []byte
	Headers map[string]string
}

const (
	// maxStreamBytes caps total streamed bytes against a runaway upstream.
	maxStreamBytes = 64 * 1024 * 1024
	// maxStreamErrorBody bounds a non-2xx stream body read for the classifier.
	maxStreamErrorBody = 64 * 1024
	// streamHeadTimeout is ProviderStreamKit's 180 s. AsyncHTTPClient applies
	// the deadline only until the response head arrives, so a long stream may
	// run past it.
	streamHeadTimeout = 180 * time.Second
)

type streamRequest struct {
	url     string
	headers [][2]string
	body    []byte
}

type streamFraming string

const (
	framingSSE    streamFraming = "\n\n"
	framingNDJSON streamFraming = "\n"
)

// runStream is ProviderStreamKit.run: it executes the request and hands each
// framed record to process until process reports the upstream finished.
//
// HTTP and network failures surface as *ProviderError so the transport can fail
// over while nothing has been yielded. Frames are split on bytes, so a
// multibyte character cut across two reads is never corrupted (Swift decodes
// each read separately and repairs it to U+FFFD).
func runStream(
	ctx context.Context,
	client *http.Client,
	kind ProviderKind,
	headTimeout time.Duration,
	framing streamFraming,
	makeRequest func(context.Context) (streamRequest, error),
	process func(record string, yield func(Chunk)) (bool, error),
) Stream {
	return func(yield func(Chunk, error) bool) {
		ctx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		fail := func(err error) { yield(Chunk{}, err) }

		request, err := makeRequest(ctx)
		if err != nil {
			fail(err)
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, request.url, bytes.NewReader(request.body))
		if err != nil {
			fail(Network(kind, opaqueNetworkError{redactURLError(err).Error()}))
			return
		}
		req.Header.Set("Content-Type", "application/json")
		for _, h := range request.headers {
			req.Header.Add(h[0], h[1])
		}

		timer := time.AfterFunc(headTimeout, func() { cancel(errors.New("stream deadline exceeded")) })
		resp, err := client.Do(req)
		timer.Stop()
		if err != nil {
			fail(Network(kind, opaqueNetworkError{redactURLError(err).Error()}))
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxStreamErrorBody))
			if readErr != nil {
				fail(readErr) // Swift lets the body read error escape unwrapped.
				return
			}
			perr := Classify(kind, resp.StatusCode, body)
			slog.ErrorContext(ctx, "llm stream upstream failed", "provider", kind, "reason", perr.ReasonCode(), "status", resp.StatusCode)
			fail(perr)
			return
		}

		stopped := false
		emit := func(c Chunk) {
			if !stopped && !yield(c, nil) {
				stopped = true
			}
		}
		var buffer []byte
		total := 0
		read := make([]byte, 32*1024)
		separator := []byte(framing)
		for !stopped {
			n, readErr := resp.Body.Read(read)
			if n > 0 {
				total += n
				if total > maxStreamBytes {
					fail(Transient(kind, 0, strPtr(fmt.Sprintf("stream exceeded %d bytes", maxStreamBytes))))
					return
				}
				buffer = append(buffer, read[:n]...)
				for {
					i := bytes.Index(buffer, separator)
					if i < 0 {
						break
					}
					record := string(buffer[:i])
					buffer = buffer[i+len(separator):]
					done, err := process(record, emit)
					if err != nil {
						if !stopped {
							fail(err)
						}
						return
					}
					if done || stopped {
						return
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				if ctx.Err() != nil {
					return // Swift checks Task.isCancelled and finishes quietly.
				}
				fail(readErr) // a body read error is not wrapped in Swift either
				return
			}
		}
		if stopped || len(buffer) == 0 {
			return
		}
		// A trailing record that arrived without its terminator.
		if _, err := process(string(buffer), emit); err != nil && !stopped {
			fail(err)
		}
	}
}

// bufferedStream is the default chatStream: buffer the completion and emit it
// as one terminal chunk.
func bufferedStream(call func() ([]byte, error)) Stream {
	return func(yield func(Chunk, error) bool) {
		data, err := call()
		if err != nil {
			yield(Chunk{}, err)
			return
		}
		yield(Chunk{Delta: ExtractContent(data), FinishReason: "stop"}, nil)
	}
}

// WithStreamFlag sets "stream": true, leaving a payload it cannot parse alone.
func WithStreamFlag(payload []byte) []byte {
	dict, ok := decodeObject(payload)
	if !ok {
		return payload
	}
	dict["stream"] = true
	out, err := encodeJSON(dict)
	if err != nil {
		return payload
	}
	return out
}

// ProcessOpenAIRecord parses one OpenAI-compatible SSE record. It handles
// [DONE], inline {"error": ...} payloads and skips named non-content events such
// as hermes.tool.progress. It returns true when the upstream signalled the end.
func ProcessOpenAIRecord(record string, kind ProviderKind, yield func(Chunk)) (bool, error) {
	eventName, hasEvent := "", false
	for _, line := range swiftLines(record) {
		if strings.HasPrefix(line, "event:") {
			eventName, hasEvent = trimWhitespaces(line[6:]), true
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := trimWhitespaces(line[5:])
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			return true, nil
		}
		if hasEvent && eventName != "message" {
			continue
		}
		obj, ok := decodeObject([]byte(payload))
		if !ok {
			continue
		}
		if errObj, ok := obj["error"].(map[string]any); ok {
			message, ok := errObj["message"].(string)
			if !ok {
				message = "upstream stream error"
			}
			return false, Transient(kind, 0, &message)
		}
		choices, ok := objects(obj["choices"])
		if !ok || len(choices) == 0 {
			continue
		}
		delta := ""
		if d, ok := choices[0]["delta"].(map[string]any); ok {
			delta, _ = d["content"].(string)
		}
		finish, hasFinish := choices[0]["finish_reason"].(string)
		if delta != "" || hasFinish {
			yield(Chunk{Delta: delta, FinishReason: finish})
		}
		if hasFinish {
			return true, nil
		}
	}
	return false, nil
}

// ExtractContent pulls choices[0].message.content out of a buffered response.
func ExtractContent(data []byte) string {
	message, ok := AssistantMessage(data)
	if !ok {
		return ""
	}
	content, _ := message["content"].(string)
	return content
}
