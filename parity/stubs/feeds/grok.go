package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A tenant's Hermes container, on the port HermesContainerHandle.baseURL fixes.
// parity/seed/grok.sql names containers that are aliases of this service.
//
//	POST /v1/chat/completions  /v1/grok/chat and /vision
//	POST /v1/responses         /v1/grok/x-search
//
// By default the answer text is what the stub received: the Host header, the bearer
// and the canonical JSON of the body. So a difference in the upstream URL, the key or
// the payload (a snake_case key, an omitted nil, a default model) shows up as a body
// difference in the route's response.
//
// [[grok:MODE]] anywhere in the request body changes the answer, for the malformed
// and failing replies the proxy has to map:
//
//	a key of grokBodies   200 with exactly that body
//	status:NNN            that status, with a small JSON body
//	ok-201                201 with a valid body
//	oversize              200 with one byte over the proxy's 8 MiB limit
//	oversize-500          500 with the same
//	short-body            Content-Length larger than the bytes sent, then a close
//	hangup                closes the connection without a response
//	reset                 the same with a TCP reset
//	garbage               a reply that isn't HTTP
//	garbage-header        a valid status line, then a header line with no colon
//	redirect:NNN          that status, pointing at /redirected
//	redirect-loop         302 to itself
//	redirect-no-location  302 with no Location
//	redirect-scheme       302 to ftp://
//	redirect-other-host   307 to the other container, same path on another origin
//	redirect-chain:N      N redirects in a row before an answer
const grokStubAddr = ":8642"

var grokDirective = regexp.MustCompile(`\[\[grok:([a-z0-9:-]+)\]\]`)

const (
	grokChoice   = `"choices":[{"message":{"content":"fixed"}}]`
	grokMaxReply = 8 << 20
)

var grokBodies = map[string]string{
	// Chat completions.
	"no-model":           `{` + grokChoice + `}`,
	"null-model":         `{"model":null,` + grokChoice + `}`,
	"model-number":       `{"model":4,` + grokChoice + `}`,
	"no-choices":         `{"model":"m"}`,
	"null-choices":       `{"model":"m","choices":null}`,
	"empty-choices":      `{"model":"m","choices":[]}`,
	"no-message":         `{"model":"m","choices":[{}]}`,
	"null-content":       `{"model":"m","choices":[{"message":{"content":null}}]}`,
	"no-content":         `{"model":"m","choices":[{"message":{}}]}`,
	"content-parts":      `{"model":"m","choices":[{"message":{"content":[{"type":"text","text":"x"}]}}]}`,
	"bad-second-choice":  `{"model":"m","choices":[{"message":{"content":"a"}},{"message":{}}]}`,
	"two-choices":        `{"model":"m","choices":[{"message":{"content":"a"}},{"message":{"content":"b"}}]}`,
	"empty-content":      `{"model":"m","choices":[{"message":{"content":""}}]}`,
	"null-usage":         `{"model":"m",` + grokChoice + `,"usage":null}`,
	"usage-partial":      `{"model":"m",` + grokChoice + `,"usage":{"prompt_tokens":1}}`,
	"usage-string":       `{"model":"m",` + grokChoice + `,"usage":{"prompt_tokens":"1","completion_tokens":2}}`,
	"usage-integral":     `{"model":"m",` + grokChoice + `,"usage":{"prompt_tokens":1.0,"completion_tokens":2e0}}`,
	"usage-fraction":     `{"model":"m",` + grokChoice + `,"usage":{"prompt_tokens":1.5,"completion_tokens":2}}`,
	"usage-camel":        `{"model":"m",` + grokChoice + `,"usage":{"promptTokens":3,"completionTokens":4}}`,
	"usage-odd-case":     `{"model":"m",` + grokChoice + `,"usage":{"PROMPT_TOKENS":5,"Completion__Tokens":6}}`,
	"usage-extra":        `{"model":"m",` + grokChoice + `,"usage":{"prompt_tokens":7,"completion_tokens":8,"total_tokens":15}}`,
	"upper-choices":      `{"model":"m","CHOICES":[{"message":{"content":"a"}}]}`,
	"leading-underscore": `{"_model":"x",` + grokChoice + `}`,
	"not-json":           `nope`,
	"empty":              ``,
	"array":              `[]`,
	"null":               `null`,
	"string":             `"text"`,
	"truncated-json":     `{"model":"m",`,
	// Responses, for x_search.
	"search-empty":         `{}`,
	"search-nulls":         `{"model":null,"output":null,"citations":null}`,
	"search-full":          `{"model":"m","output":"text","citations":[{"url":"https://x.test/1","title":"T","published_at":"2026-01-02T03:04:05Z"},{"url":"https://x.test/2"},{"url":"https://x.test/3","title":null,"published_at":null}]}`,
	"search-output-parts":  `{"model":"m","output":[{"type":"message"}]}`,
	"search-citation-bare": `{"citations":["https://x.test/1"]}`,
	"search-no-url":        `{"citations":[{"title":"T"}]}`,
	"search-null-url":      `{"citations":[{"url":null}]}`,
	"search-date-offset":   `{"citations":[{"url":"u","published_at":"2026-01-02T03:04:05+02:00"}]}`,
	"search-date-fraction": `{"citations":[{"url":"u","published_at":"2026-01-02T03:04:05.750Z"}]}`,
	"search-date-camel":    `{"citations":[{"url":"u","publishedAt":"2026-01-02T03:04:05Z"}]}`,
	"search-date-only":     `{"citations":[{"url":"u","published_at":"2026-01-02"}]}`,
	"search-date-garbage":  `{"citations":[{"url":"u","published_at":"yesterday"}]}`,
	"search-date-number":   `{"citations":[{"url":"u","published_at":1767323045}]}`,
}

func grokEcho(r *http.Request, raw []byte) string {
	var v any
	canonical := raw
	if json.Unmarshal(raw, &v) == nil {
		canonical, _ = json.Marshal(v)
	}
	return fmt.Sprintf("host=%s auth=%s body=%s", r.Host, r.Header.Get("Authorization"), canonical)
}

func grokWrite(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// grokHandler answers with reply, the route's normal body for the text received,
// unless the request holds a directive.
func grokHandler(reply func(model, text string) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var mode string
		if m := grokDirective.FindSubmatch(raw); m != nil {
			mode = string(m[1])
		}
		if body, ok := grokBodies[mode]; ok {
			grokWrite(w, http.StatusOK, body)
			return
		}
		verb, arg, _ := strings.Cut(mode, ":")
		switch verb {
		case "status":
			code, _ := strconv.Atoi(arg)
			grokWrite(w, code, `{"error":"stub"}`)
			return
		case "oversize", "oversize-500":
			status := http.StatusOK
			if verb == "oversize-500" {
				status = http.StatusInternalServerError
			}
			grokWrite(w, status, strings.Repeat(" ", grokMaxReply+1))
			return
		case "short-body":
			w.Header().Set("Content-Length", "1000")
			_, _ = io.WriteString(w, `{"model":`)
			grokHangUp(w)
			return
		case "hangup":
			grokHangUp(w)
			return
		case "redirect":
			code, _ := strconv.Atoi(arg)
			w.Header().Set("Location", "/redirected")
			w.WriteHeader(code)
			return
		case "redirect-loop":
			w.Header().Set("Location", r.URL.Path)
			w.WriteHeader(http.StatusFound)
			return
		case "redirect-no-location":
			w.WriteHeader(http.StatusFound)
			return
		case "redirect-scheme":
			w.Header().Set("Location", "ftp://grok-hermes-ben/x")
			w.WriteHeader(http.StatusFound)
			return
		case "redirect-other-host":
			w.Header().Set("Location", "http://grok-hermes-ben"+grokStubAddr+"/redirected")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		case "redirect-chain":
			w.Header().Set("Location", "/chain/"+arg)
			w.WriteHeader(http.StatusFound)
			return
		case "reset":
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
				_ = conn.Close()
			}
			return
		case "garbage", "garbage-header":
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				reply := "this is not http\r\n\r\n"
				if verb == "garbage-header" {
					reply = "HTTP/1.1 200 OK\r\nno colon here\r\nContent-Length: 0\r\n\r\n"
				}
				_, _ = io.WriteString(conn, reply)
				_ = conn.Close()
			}
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &req)
		b, _ := json.Marshal(reply(req.Model, grokEcho(r, raw)))
		status := http.StatusOK
		if verb == "ok-201" {
			status = http.StatusCreated
		}
		grokWrite(w, status, string(b))
	}
}

func grokHangUp(w http.ResponseWriter) {
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.Flush()
		if conn, _, err := rc.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

func grokChatReply(model, text string) any {
	return map[string]any{
		"id": "stub-grok", "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

func grokResponsesReply(model, text string) any {
	return map[string]any{
		"model": model, "output": text,
		"citations": []any{map[string]any{"url": "https://x.test/status/1", "title": "A post", "published_at": "2026-01-02T03:04:05Z"}},
	}
}

// startGrokStub serves the Hermes side on its own port, as a container does.
func startGrokStub() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", grokHandler(grokChatReply))
	mux.HandleFunc("POST /v1/responses", grokHandler(grokResponsesReply))
	// What a followed redirect lands on: the method and what survived of the bearer,
	// content type and body, as the answer text, in the chat shape.
	redirected := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		text := fmt.Sprintf("redirected host=%s method=%s auth=%s ct=%s body=%d", r.Host, r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), len(raw))
		b, _ := json.Marshal(grokChatReply("redirected", text))
		grokWrite(w, http.StatusOK, string(b))
	}
	mux.HandleFunc("/redirected", redirected)
	mux.HandleFunc("/chain/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		if n <= 1 {
			redirected(w, r)
			return
		}
		w.Header().Set("Location", "/chain/"+strconv.Itoa(n-1))
		w.WriteHeader(http.StatusFound)
	})
	srv := &http.Server{Addr: grokStubAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { log.Fatal(srv.ListenAndServe()) }()
}
