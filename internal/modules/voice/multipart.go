package voice

import (
	"bytes"
	"errors"
	"lumina/internal/swiftstr"
	"strings"
	"unicode/utf8"
)

// defaultMaxParts is MultipartFormParser's maxParts: the OpenAI audio endpoints send
// at most a handful.
const defaultMaxParts = 32

// Failures of parseMultipart, MultipartFormParser.ParseError. The route answers them
// all the same way.
var (
	errNoOpeningDelimiter = errors.New("multipart: no opening delimiter")
	errUnterminated       = errors.New("multipart: unterminated")
	errMalformedPart      = errors.New("multipart: malformed part")
	errTooManyParts       = errors.New("multipart: too many parts")
)

// formPart is MultipartFormParser.Part. Filename and ContentType are nil when absent.
type formPart struct {
	name        string
	filename    *string
	contentType *string
	body        []byte
}

// multipartBoundary is MultipartFormParser.boundary(fromContentType:): the boundary
// parameter of any Content-Type, quoted or not. It doesn't look at the media type, so
// `application/json; boundary=x` has one, as in Swift.
func multipartBoundary(header string) (string, bool) {
	params := splitNonEmpty(header, ";")
	if len(params) == 0 {
		return "", false
	}
	for _, param := range params[1:] {
		key, value, ok := strings.Cut(swiftstr.TrimWhitespaces(param), "=")
		if !ok || strings.ToLower(swiftstr.TrimWhitespaces(key)) != "boundary" {
			continue
		}
		value = unquote(swiftstr.TrimWhitespaces(value))
		return value, value != ""
	}
	return "", false
}

// splitNonEmpty is Swift's split(separator:), which drops empty pieces.
func splitNonEmpty(s, sep string) []string {
	var out []string
	for part := range strings.SplitSeq(s, sep) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// unquote removes one pair of surrounding double quotes.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// parseMultipart is MultipartFormParser.parse: a hand-rolled RFC 7578 reader, because
// Swift's quirks are the contract here and mime/multipart has other ones. Only a CRLF
// followed by the delimiter ends a part, so binary audio holding the boundary string
// survives.
func parseMultipart(body []byte, boundary string, maxParts int) ([]formPart, error) {
	crlf := []byte("\r\n")
	delimiter := append([]byte("--"), boundary...)
	partEnd := append(append([]byte(nil), crlf...), delimiter...)

	// Skip any preamble before the first delimiter.
	cursor := bytes.Index(body, delimiter)
	if cursor < 0 {
		return nil, errNoOpeningDelimiter
	}

	var parts []formPart
	for {
		cursor += len(delimiter)

		// `--boundary--` ends the body. Anything after it is epilogue.
		if cursor+1 < len(body) && body[cursor] == '-' && body[cursor+1] == '-' {
			return parts, nil
		}
		// Otherwise a CRLF closes the delimiter line, after any linear whitespace.
		for cursor < len(body) && (body[cursor] == ' ' || body[cursor] == '\t') {
			cursor++
		}
		if cursor+1 >= len(body) || body[cursor] != '\r' || body[cursor+1] != '\n' {
			return nil, errUnterminated
		}
		cursor += 2

		if len(parts) >= maxParts {
			return nil, errTooManyParts
		}

		// Headers run to the first blank line.
		separator := bytes.Index(body[cursor:], []byte("\r\n\r\n"))
		if separator < 0 {
			return nil, errMalformedPart
		}
		headerBytes := body[cursor : cursor+separator]
		bodyStart := cursor + separator + 4

		// The body runs to the CRLF that precedes the next delimiter.
		length := bytes.Index(body[bodyStart:], partEnd)
		if length < 0 {
			return nil, errUnterminated
		}
		bodyEnd := bodyStart + length

		headers := parsePartHeaders(headerBytes)
		disposition, ok := headers["content-disposition"]
		if !ok {
			return nil, errMalformedPart
		}
		name, ok := headerParameter("name", disposition)
		if !ok {
			return nil, errMalformedPart
		}
		part := formPart{name: name, body: body[bodyStart:bodyEnd]}
		if filename, ok := headerParameter("filename", disposition); ok {
			part.filename = &filename
		}
		if contentType, ok := headers["content-type"]; ok {
			part.contentType = &contentType
		}
		parts = append(parts, part)

		cursor = bodyEnd + len(crlf)
	}
}

// parsePartHeaders maps lowercased names to values, keeping the first of duplicates.
// Headers that aren't UTF-8 read as none at all, as String(bytes:encoding:) gives nil.
func parsePartHeaders(raw []byte) map[string]string {
	headers := map[string]string{}
	if !utf8.Valid(raw) {
		return headers
	}
	for line := range strings.SplitSeq(string(raw), "\r\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(swiftstr.TrimWhitespaces(key))
		if _, seen := headers[key]; !seen {
			headers[key] = swiftstr.TrimWhitespaces(value)
		}
	}
	return headers
}

// headerParameter pulls key="value" or key=value out of a header value. It splits on
// every semicolon, so a quoted value can't hold one, as in Swift.
func headerParameter(key, header string) (string, bool) {
	for _, param := range splitNonEmpty(header, ";") {
		found, value, ok := strings.Cut(swiftstr.TrimWhitespaces(param), "=")
		if ok && strings.ToLower(swiftstr.TrimWhitespaces(found)) == key {
			return unquote(swiftstr.TrimWhitespaces(value)), true
		}
	}
	return "", false
}
