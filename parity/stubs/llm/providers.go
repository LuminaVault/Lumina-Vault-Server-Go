package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// stubTranscript is the fixed text of every transcription.
	stubTranscript = "stub transcript"
	// maxAudioBytes bounds an upload read into memory. Swift caps /v1/transcribe at 10 MB.
	maxAudioBytes = 32 << 20
)

// audioExtensions are the file names OpenAICompatibleTranscribeAdapter can send
// that the in-cluster service decodes: OGG/Opus, m4a, mp3, wav and webm, with the
// adapter's other spellings of the same containers. Anything else is the caller's
// mistake (audio.bin means the adapter couldn't name the type) and answers 400.
var audioExtensions = map[string]bool{
	".ogg": true, ".opus": true, ".m4a": true, ".mp4": true,
	".mp3": true, ".mpga": true, ".wav": true, ".webm": true, ".flac": true,
}

// A directive is only looked for near the start of the audio, as any upload's bytes
// can hold anything.
var audioDirectiveRe = regexp.MustCompile(`\[\[stub:(error|reply):[^\]]*\]\]`)

const audioDirectiveWindow = 4096

// mountProviders adds the upstreams with no OpenAI or Anthropic chat shape:
//
//	POST /v1/audio/transcriptions   Whisper. Multipart file and model; {"text"}, or
//	                                the verbose shape on response_format=verbose_json.
//	                                A directive in the first bytes of the audio works
//	POST /v1/audio/speech           OpenAI's speech shape. The audio is a digest of the
//	                                request, so a different one shows up as other bytes.
//	                                A directive in the input works
//	POST /api/chat                  Ollama, NDJSON unless stream is false
//	GET  /api/tags                  Ollama's model list
//
// Gemini isn't here: Swift hard-codes its host for chat and for the model list,
// and so does Anthropic's model list, so no compose setting can bring them to a
// stub. Refusing those hosts (docker-compose.parity.yml) makes them fail alike.
// /v1/embeddings isn't here because parity runs the deterministic provider.
func (s *stub) mountProviders(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/audio/transcriptions", s.transcribe)
	mux.HandleFunc("POST /v1/audio/speech", s.speech)
	mux.HandleFunc("POST /api/chat", s.ollamaChat)
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, _ *http.Request) {
		s.count("/api/tags")
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{map[string]any{"name": "llama3:8b"}}})
	})
}

func openAIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error"}})
}

// speech answers what a server sends for /v1/tts. It ignores Authorization: Swift sends
// a key and Go, which only talks to a self-hosted service, doesn't.
func (s *stub) speech(w http.ResponseWriter, r *http.Request) {
	s.count("/v1/audio/speech")
	var body struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		ResponseFormat string `json:"response_format"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAudioBytes)).Decode(&body); err != nil {
		openAIError(w, http.StatusBadRequest, "bad JSON")
		return
	}
	if body.Model == "" || body.Input == "" || body.Voice == "" {
		openAIError(w, http.StatusBadRequest, "model, input and voice are required")
		return
	}
	if a := decide([]message{userMessage(body.Input)}); a.errorCode != 0 {
		openAIError(w, a.errorCode, "stub error")
		return
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{body.Model, body.Input, body.Voice, body.ResponseFormat}, "\x00")))
	w.Header().Set("Content-Type", "audio/mpeg")
	_, _ = w.Write(append([]byte("ID3"), sum[:]...))
}

// transcribe reads the form as a stream, so it needs no temporary files.
func (s *stub) transcribe(w http.ResponseWriter, r *http.Request) {
	s.count("/v1/audio/transcriptions")
	form, err := r.MultipartReader()
	if err != nil {
		openAIError(w, http.StatusBadRequest, "multipart form required")
		return
	}
	var audio []byte
	var filename, model, format string
	for {
		part, err := form.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			openAIError(w, http.StatusBadRequest, "bad multipart form")
			return
		}
		value, err := io.ReadAll(io.LimitReader(part, maxAudioBytes+1))
		if err != nil || len(value) > maxAudioBytes {
			openAIError(w, http.StatusRequestEntityTooLarge, "form part too large")
			return
		}
		switch part.FormName() {
		case "file":
			audio, filename = value, part.FileName()
		case "model":
			model = strings.TrimSpace(string(value))
		case "response_format":
			format = strings.TrimSpace(string(value))
		}
	}
	if len(audio) == 0 || model == "" {
		openAIError(w, http.StatusBadRequest, "file and model are required")
		return
	}
	if !audioExtensions[strings.ToLower(filepath.Ext(filename))] {
		openAIError(w, http.StatusBadRequest, "unsupported audio format")
		return
	}
	text := stubTranscript
	if m := audioDirectiveRe.Find(audio[:min(len(audio), audioDirectiveWindow)]); m != nil {
		a := decide([]message{userMessage(string(m))})
		if a.errorCode != 0 {
			openAIError(w, a.errorCode, "stub error")
			return
		}
		text = a.text
	}
	if format != "verbose_json" {
		writeJSON(w, http.StatusOK, map[string]any{"text": text})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task": "transcribe", "text": text, "language": "en", "duration": 1.5,
		"segments": []any{map[string]any{"id": 0, "start": 0.0, "end": 1.5, "text": text, "avg_logprob": -0.1}},
	})
}

func (s *stub) ollamaChat(w http.ResponseWriter, r *http.Request) {
	s.count("/api/chat")
	var req struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
		Stream   *bool     `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
		return
	}
	a := decide(req.Messages)
	if a.errorCode != 0 {
		writeJSON(w, a.errorCode, map[string]any{"error": "stub error"})
		return
	}
	line := func(content string, done bool) map[string]any {
		out := map[string]any{"model": req.Model, "created_at": "2023-11-14T22:13:20Z", "message": map[string]any{"role": "assistant", "content": content}, "done": done}
		if done {
			out["done_reason"], out["prompt_eval_count"], out["eval_count"] = "stop", 10, 5
		}
		return out
	}
	if req.Stream != nil && !*req.Stream {
		writeJSON(w, http.StatusOK, line(a.text, true))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	rc := http.NewResponseController(w)
	emit := func(v map[string]any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "%s\n", b)
		_ = rc.Flush()
	}
	for _, p := range pieces(a.text) {
		emit(line(p, false))
	}
	emit(line("", true))
}
