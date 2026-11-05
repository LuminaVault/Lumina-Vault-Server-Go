// Package openaierror is the Go port of Swift's OpenAIErrorEnvelope and
// OpenAIErrorEnvelopeMiddleware (Sources/App/LLM/OpenAIAudio): every failure on
// /v1/audio/* in OpenAI's error shape.
//
// The callers are OpenAI SDK clients. The SDK builds its message from
// body["error"]["message"] and Hermes relays that to the end user, so the envelope
// decides whether someone on Telegram reads "you've used today's voice" or
// `Error code: 429 - {...}`.
package openaierror

import (
	"errors"
	"lumina/internal/wire"
	"maps"
	"net/http"

	"github.com/labstack/echo/v5"
)

// Payload is OpenAIErrorEnvelope.Payload. A nil Code or Param is left out.
type Payload struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    *string `json:"code,omitempty"`
	Param   *string `json:"param,omitempty"`
}

// Envelope is OpenAIErrorEnvelope.
type Envelope struct {
	Error Payload `json:"error"`
}

func newEnvelope(message, typ, code string) Envelope {
	p := Payload{Message: message, Type: typ}
	if code != "" {
		p.Code = &code
	}
	return Envelope{Error: p}
}

// InvalidRequest is .invalidRequest(_:code:). An empty code is left out.
func InvalidRequest(message, code string) Envelope {
	return newEnvelope(message, "invalid_request_error", code)
}

// RateLimit is .rateLimit(_:).
func RateLimit(message string) Envelope {
	return newEnvelope(message, "rate_limit_error", "rate_limit_exceeded")
}

// InsufficientQuota is .insufficientQuota(_:).
func InsufficientQuota(message string) Envelope {
	return newEnvelope(message, "insufficient_quota", "insufficient_quota")
}

// Authentication is .authentication(_:).
func Authentication(message string) Envelope {
	return newEnvelope(message, "authentication_error", "invalid_api_key")
}

// Server is .server(_:code:). An empty code is left out.
func Server(message, code string) Envelope {
	return newEnvelope(message, "server_error", code)
}

// ForStatus is OpenAIErrorEnvelopeMiddleware.envelope(status:message:): status
// mapped onto OpenAI's `type` taxonomy, keeping the message if the thrower had one.
func ForStatus(status int, message *string) Envelope {
	text := func(fallback string) string {
		if message != nil {
			return *message
		}
		return fallback
	}
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return Authentication(text("Not authorized for this audio endpoint."))
	case status == http.StatusPaymentRequired:
		// Reached only when the tier has no voice allowance at all.
		return InsufficientQuota(text("Voice is not available on this plan."))
	case status == http.StatusRequestEntityTooLarge:
		return InvalidRequest(text("Audio file is too large."), "file_too_large")
	case status == http.StatusUnsupportedMediaType:
		return InvalidRequest(text("Unsupported audio format."), "unsupported_format")
	case status == http.StatusTooManyRequests:
		return RateLimit(text("You have reached the voice limit for now. Try again later."))
	case status >= 400 && status < 500:
		return InvalidRequest(text(reasonPhrase(status)), "")
	case status == http.StatusNotImplemented:
		// Spoken replies, until that milestone ships.
		return Server(text("This audio capability is not enabled."), "not_implemented")
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		return Server(text("The speech service is temporarily unavailable."), "upstream_error")
	}
	return Server(text(reasonPhrase(status)), "")
}

// reasonPhrase is HTTPResponse.Status.reasonPhrase. swift-http-types follows RFC
// 9110 where net/http keeps the older names of these two. (413 has its own message.)
func reasonPhrase(status int) string {
	switch status {
	case http.StatusRequestedRangeNotSatisfiable:
		return "Range Not Satisfiable"
	case http.StatusUnprocessableEntity:
		return "Unprocessable Content"
	}
	return http.StatusText(status)
}

// Write sends env with status, as OpenAIErrorEnvelopeMiddleware.encode does.
func Write(w http.ResponseWriter, status int, env Envelope) error {
	b, err := wire.MarshalFoundation(env)
	if err != nil {
		return err
	}
	w.Header().Set(echo.HeaderContentType, wire.ContentTypeJSON)
	w.WriteHeader(status)
	_, err = w.Write(b)
	return err
}

// Middleware is OpenAIErrorEnvelopeMiddleware. Mount it outermost on the group, so
// it wraps the authenticator, the entitlement gate and the rate limits.
//
// It catches what Swift's does in both shapes: an error a middleware returns (a rate
// limit, Swift's throw), which keeps its message; and a failure status a handler
// writes itself (the entitlement paywall, Swift's returned Response), whose body is
// discarded. Retry-After survives, and every other header set below this middleware
// goes. An unknown error passes through, so it stays the opaque 500 rather than being
// dressed up as a well-understood OpenAI failure.
func Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			out := c.Response()
			before := out.Header().Clone()
			w := &failureWriter{ResponseWriter: out}
			c.SetResponse(w)
			defer c.SetResponse(out)

			err := next(c)
			status, message, ok := failure(c, w, err)
			if !ok {
				return err
			}
			retryAfter := out.Header().Get("Retry-After")
			maps.DeleteFunc(out.Header(), func(string, []string) bool { return true })
			maps.Copy(out.Header(), before)
			if retryAfter != "" {
				out.Header().Set("Retry-After", retryAfter)
			}
			return Write(out, status, ForStatus(status, message))
		}
	}
}

// failure decides whether a finished request gets the envelope, and with what.
func failure(c *echo.Context, w *failureWriter, err error) (status int, message *string, ok bool) {
	if w.failed != 0 {
		return w.failed, nil, true
	}
	if w.wroteHeader || err == nil {
		return 0, nil, false
	}
	var we *wire.Error
	if errors.As(err, &we) {
		return we.Status, &we.Message, true
	}
	// wire.ResponseError is how a gate writes a body of its own. Let it write here,
	// so that it counts as the returned Response it stands for.
	var re wire.ResponseError
	if errors.As(err, &re) {
		if writeErr := re.WriteResponse(c); writeErr == nil && w.failed != 0 {
			return w.failed, nil, true
		}
		return 0, nil, false
	}
	// An echo status error (ErrBodyTooLarge) is HTTPError with no message.
	if code := echo.StatusCode(err); code >= 400 {
		return code, nil, true
	}
	return 0, nil, false
}

// failureWriter holds back any response whose status is 400 or above, so the
// middleware can replace it. Anything else passes straight through.
type failureWriter struct {
	http.ResponseWriter
	wroteHeader bool
	failed      int
}

func (w *failureWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if status >= 400 {
		w.failed = status
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *failureWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.failed != 0 {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// FlushError keeps a flush from committing a response being held back.
func (w *failureWriter) FlushError() error {
	if w.failed != 0 {
		return nil
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets echo.UnwrapResponse and ResponseController reach the real writer.
func (w *failureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
