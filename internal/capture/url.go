package capture

import (
	"lumina/internal/vaultfiles"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// URL is what the capture pipeline reads from a Foundation URL. ParseURL builds
// it. Foundation is the reference, because a capture's status code, its file
// name and the address enrichment fetches all come from this parse.
type URL struct {
	// Scheme is url.scheme, as written. Nil for a relative reference.
	Scheme *string
	// Host is url.host: percent-decoded, IDNA-encoded and without an IP
	// literal's brackets. Nil when there is no authority or it has no host.
	Host *string
	// Absolute is url.absoluteString, with invalid characters percent-encoded.
	Absolute string

	componentsHost string  // URLComponents.host, for Normalize
	port           string  // digits, or "" when absent or empty
	path           string  // percent-encoded
	query          *string // percent-encoded
}

// ParseURL is Foundation's URL(string:), which is RFC 3986 with
// encodingInvalidCharacters on: the path, query and fragment percent-encode what
// they can't hold, while the scheme, port and host refuse it. So "not a url" is a
// relative reference, and "https://example.com " is no URL at all.
//
// Read from swift-foundation's behaviour by differential test: testdata holds
// Foundation's answers (Swift 6.4.1, Darwin) and TestParseURLMatchesFoundation
// replays them. Linux runs the same parser, but the IDNA tables come from ICU
// there and from Go's x/net here, so non-ASCII hosts can differ in rare cases.
func ParseURL(s string) (*URL, bool) {
	if s == "" {
		return nil, false
	}
	u := &URL{}
	rest := s
	var out strings.Builder
	if i := strings.IndexAny(s, ":/?#"); i >= 0 && s[i] == ':' {
		scheme := s[:i]
		switch {
		case scheme == "" || validScheme(scheme):
			u.Scheme = &scheme
			out.WriteString(scheme + ":")
			rest = s[i+1:]
		case !strings.ContainsAny(scheme, "@[]"):
			return nil, false
		}
		// Otherwise, as measured: an invalid scheme holding "@", "[" or "]" makes
		// the whole string a relative reference.
	}
	if strings.HasPrefix(rest, "//") {
		authority := rest[2:]
		if i := strings.IndexAny(authority, "/?#"); i >= 0 {
			rest = authority[i:]
			authority = authority[:i]
		} else {
			rest = ""
		}
		encoded, ok := u.parseAuthority(authority)
		if !ok {
			return nil, false
		}
		out.WriteString("//" + encoded)
	}
	path, query, fragment := rest, (*string)(nil), (*string)(nil)
	if i := strings.IndexByte(path, '#'); i >= 0 {
		f := path[i+1:]
		fragment, path = &f, path[:i]
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		q := path[i+1:]
		query, path = &q, path[:i]
	}
	dirty := false
	encode := func(component string, allowed func(byte) bool) string {
		encoded := encodeComponent(component, allowed)
		dirty = dirty || encoded != component
		return encoded
	}
	u.path = encode(path, isPathByte)
	var tail strings.Builder
	tail.WriteString(u.path)
	if query != nil {
		q := encode(*query, isQueryByte)
		u.query = &q
		tail.WriteString("?" + q)
	}
	if fragment != nil {
		tail.WriteString("#" + encode(*fragment, isQueryByte))
	}
	head := out.String()
	if dirty {
		// Rebuilding the string after encoding loses an empty port's colon.
		head = strings.Replace(head, emptyPortMark, "", 1)
	}
	u.Absolute = strings.Replace(head, emptyPortMark, ":", 1) + tail.String()
	return u, true
}

// emptyPortMark stands for the ":" of an empty port until ParseURL knows whether
// absoluteString keeps it.
const emptyPortMark = "\x00port\x00"

// parseAuthority fills the host fields and returns the authority as it appears
// in absoluteString. userinfo is percent-encoded like a path, the rest refuses
// what it can't hold.
func (u *URL) parseAuthority(authority string) (string, bool) {
	var encoded strings.Builder
	var rawPort string
	hasPort, hasUserinfo, rewritten := false, false, false
	hostPort := authority
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		userinfo := encodeComponent(authority[:i], isUserinfoByte)
		rewritten = userinfo != authority[:i]
		hasUserinfo = true
		encoded.WriteString(userinfo + "@")
		hostPort = authority[i+1:]
	}
	var host string
	if strings.HasPrefix(hostPort, "[") {
		end := strings.IndexByte(hostPort, ']')
		if end < 0 {
			return "", false
		}
		literal := hostPort[1:end]
		if !validIPLiteral(literal) {
			return "", false
		}
		after := hostPort[end+1:]
		if after != "" {
			if after[0] != ':' {
				return "", false
			}
			rawPort, hasPort = after[1:], true
		}
		u.Host, u.componentsHost = &literal, "["+literal+"]"
		host = "[" + strings.ReplaceAll(literal, "%", "%25") + "]"
	} else {
		host = hostPort
		if i := strings.LastIndexByte(hostPort, ':'); i >= 0 {
			host, rawPort, hasPort = hostPort[:i], hostPort[i+1:], true
		}
		ascii, decoded, components, hasHost, ok := parseRegName(host)
		if !ok {
			return "", false
		}
		rewritten = rewritten || ascii != host
		// QUIRK: "http:///x" has a nil url.host, and "http://:80/" an empty one.
		if hasHost || (host == "" && (hasPort || hasUserinfo)) {
			u.Host = &decoded
		}
		host = ascii
		u.componentsHost = components
	}
	for i := range len(rawPort) {
		if rawPort[i] < '0' || rawPort[i] > '9' {
			return "", false
		}
	}
	u.port = rawPort
	encoded.WriteString(host)
	switch {
	case hasPort && rawPort != "":
		encoded.WriteString(":" + rawPort)
	case hasPort && !rewritten:
		encoded.WriteString(emptyPortMark)
	}
	return encoded.String(), true
}

// parseRegName validates a non-bracketed host. ascii goes in absoluteString,
// decoded is url.host, and components is URLComponents.host.
func parseRegName(host string) (ascii, decoded, components string, hasHost, ok bool) {
	if host == "" {
		return "", "", "", false, true
	}
	if !validPercents(host) {
		return "", "", "", false, false
	}
	nonASCII := false
	for i := range len(host) {
		c := host[i]
		if c >= utf8.RuneSelf {
			nonASCII = true
			continue
		}
		if !isHostByte(c) {
			return "", "", "", false, false
		}
	}
	if nonASCII {
		encoded, err := idnaProfile.ToASCII(host)
		if err != nil {
			return "", "", "", false, false
		}
		unicode, _ := idnaProfile.ToUnicode(encoded)
		return encoded, percentDecode(encoded), unicode, true, true
	}
	text := percentDecode(host)
	if !utf8.ValidString(text) {
		// url.host is nil when the bytes aren't text; the guard refuses it.
		return host, "", "", false, true
	}
	components = text
	if strings.Contains(strings.ToLower(host), "xn--") {
		if unicode, err := idnaProfile.ToUnicode(host); err == nil {
			components = unicode
		}
	}
	return host, text, components, true, true
}

var idnaProfile = idna.New(idna.MapForLookup(), idna.Transitional(false), idna.StrictDomainName(false))

func validScheme(s string) bool {
	for i := range len(s) {
		c := s[i]
		switch {
		case isAlpha(c):
		case i > 0 && (isDigit(c) || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
}

// validIPLiteral accepts what Foundation's host parse does between brackets. It
// doesn't check that the text is an address ("[zz]" parses).
func validIPLiteral(s string) bool {
	for i := range len(s) {
		c := s[i]
		if !isHostByte(c) && c != ':' {
			return false
		}
	}
	return true
}

func validPercents(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		if i+2 >= len(s) {
			return false
		}
		if !isHex(s[i+1]) || !isHex(s[i+2]) {
			return false
		}
		i += 2
	}
	return true
}

// encodeComponent is what encodingInvalidCharacters does to one component, as
// measured. A component that is already valid stays as written. One that isn't is
// encoded whole, and that includes the "%" of its valid escapes, so
// "/a%20b c" becomes "/a%2520b%20c".
func encodeComponent(s string, allowed func(byte) bool) string {
	valid := validPercents(s)
	for i := 0; valid && i < len(s); i++ {
		valid = s[i] == '%' || allowed(s[i])
	}
	if valid {
		return s
	}
	return encodeComponentStrict(s, allowed)
}

func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isHex(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case isDigit(c):
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

func isUnreserved(c byte) bool {
	return isAlpha(c) || isDigit(c) || c == '-' || c == '.' || c == '_' || c == '~'
}

func isSubDelim(c byte) bool { return strings.IndexByte("!$&'()*+,;=", c) >= 0 }

func isHostByte(c byte) bool     { return isUnreserved(c) || isSubDelim(c) || c == '%' }
func isUserinfoByte(c byte) bool { return isUnreserved(c) || isSubDelim(c) || c == ':' }
func isPathByte(c byte) bool {
	return isUnreserved(c) || isSubDelim(c) || c == ':' || c == '@' || c == '/'
}
func isQueryByte(c byte) bool { return isPathByte(c) || c == '?' }

// Normalize is URLExtractionService.normalize, the dedupe key: lowercased scheme
// and host, the default port dropped, a lone "/" path dropped, no fragment, and
// the path and query as URLComponents reads them, which is percent-decoded.
func (u *URL) Normalize() string {
	scheme := "http"
	if u.Scheme != nil {
		scheme = vaultfiles.Lowercased(*u.Scheme)
	}
	path := percentDecode(u.path)
	if path == "/" {
		path = ""
	}
	port := ""
	if u.port != "" {
		defaultPort := "80"
		if scheme == "https" {
			defaultPort = "443"
		}
		if trimmed := strings.TrimLeft(u.port, "0"); trimmed != defaultPort {
			if trimmed == "" {
				trimmed = "0"
			}
			port = ":" + trimmed
		}
	}
	query := ""
	if u.query != nil && *u.query != "" {
		query = "?" + percentDecode(*u.query)
	}
	return scheme + "://" + vaultfiles.Lowercased(u.componentsHost) + port + path + query
}
