package capture

import (
	"lumina/internal/vaultfiles"
	"strconv"
	"strings"
)

// IsPublic is URLEnricherGuard.isPublic, the SSRF check on captured links: http
// or https, a host, and not a host that names an internal or private address.
//
// It reads the URL's text and resolves nothing, so it is only the cheap first
// check that turns an obviously internal link into a 400. The real SSRF defense
// is the fetch client: NewEnrichmentService defaults to ssrfguard's strict
// client, which checks every dialed address after DNS and on every redirect hop.
//
// QUIRKS, kept on purpose, all from reading Swift:
//   - The IPv6 checks run on every host, so any name starting with "fc" or "fd" is
//     refused: https://fcbarcelona.com and https://fdroid.org answer 400.
//   - The IPv4 check drops non-numeric labels first, so 10.0.0.1.example.com
//     counts as 10.0.0.1 and is refused.
//   - Other spellings of a private address pass: 127.1, 2130706433, 0x7f.0.0.1 and
//     [::ffff:127.0.0.1] all count as public.
func IsPublic(u *URL) bool {
	if u.Scheme == nil {
		return false
	}
	if scheme := vaultfiles.Lowercased(*u.Scheme); scheme != "http" && scheme != "https" {
		return false
	}
	if u.Host == nil || *u.Host == "" {
		return false
	}
	host := vaultfiles.Lowercased(*u.Host)
	switch host {
	case "localhost", "ip6-localhost", "ip6-loopback", "metadata.google.internal":
		return false
	}
	if strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return false
	}
	return !isPrivateNumericHost(host)
}

func isPrivateNumericHost(host string) bool {
	stripped := strings.Trim(host, "[]")

	var parts []uint64
	for label := range strings.SplitSeq(stripped, ".") {
		if n, ok := parseUInt8(label); ok {
			parts = append(parts, n)
		}
	}
	if len(parts) == 4 {
		a, b := parts[0], parts[1]
		switch {
		case a == 10, a == 127, a == 0:
			return true
		case a == 169 && b == 254:
			return true
		case a == 172 && b >= 16 && b <= 31:
			return true
		case a == 192 && b == 168:
			return true
		case a == 100 && b >= 64 && b <= 127:
			return true
		}
	}

	lower := vaultfiles.Lowercased(stripped)
	if lower == "::1" || lower == "::" {
		return true
	}
	return strings.HasPrefix(lower, "fc") || strings.HasPrefix(lower, "fd") || strings.HasPrefix(lower, "fe80")
}

// parseUInt8 is Swift's UInt8(_: String): ASCII digits only, one optional sign,
// and "-0" is zero.
func parseUInt8(s string) (uint64, bool) {
	negative := strings.HasPrefix(s, "-")
	digits := strings.TrimPrefix(strings.TrimPrefix(s, "+"), "-")
	if len(s)-len(digits) > 1 || digits == "" || digits[0] == '+' {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 8)
	if err != nil || (negative && n != 0) {
		return 0, false
	}
	return n, true
}
