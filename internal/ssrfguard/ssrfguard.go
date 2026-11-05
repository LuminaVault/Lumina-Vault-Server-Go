// Package ssrfguard is the Go port of Swift's SSRFGuard
// (Sources/App/Settings/SSRFGuard.swift), the allowlist for user-supplied Hermes
// endpoints: a URL is rejected if its scheme isn't allowed or its host resolves
// into a loopback, private or link-local range. Call Validate before every
// outbound request as well as when the URL is saved: re-resolving each time is
// the DNS rebinding defense.
//
// internal/modules/marketplace has its own copy, fixed to the one configuration
// its broker uses. This one takes every setting.
package ssrfguard

import (
	"context"
	"lumina/internal/swiftstr"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Kind is a SSRFGuard.Rejection case.
type Kind string

const (
	InvalidURL       Kind = "invalidURL"
	SchemeNotAllowed Kind = "schemeNotAllowed"
	HostMissing      Kind = "hostMissing"
	PrivateAddress   Kind = "privateAddress"
	Loopback         Kind = "loopback"
	LinkLocal        Kind = "linkLocal"
	Unresolvable     Kind = "unresolvable"
)

// Rejection is why a URL was refused.
type Rejection struct {
	Kind  Kind
	Value string
}

// Error is String(describing:) of the Swift enum case, for example
// `loopback("127.0.0.1")`. It reaches clients in the 502 message of
// hermesresolve, so the format is part of the wire contract.
func (r *Rejection) Error() string {
	if r.Kind == InvalidURL || r.Kind == HostMissing {
		return string(r.Kind)
	}
	return string(r.Kind) + "(" + strconv.Quote(r.Value) + ")"
}

// Resolver turns a host into IP literals.
type Resolver func(ctx context.Context, host string) ([]string, error)

// Guard is the configuration. The zero value is the strictest setting except
// that plain http is allowed; see the fields.
type Guard struct {
	// AllowPrivateRanges opens RFC1918, loopback and link-local targets. Local
	// development only (byoHermes.allowPrivate).
	AllowPrivateRanges bool
	// RequireHTTPS rejects http:// (byoHermes.requireHttps).
	RequireHTTPS bool
	// AllowTailnetHTTP waives RequireHTTPS for hosts whose every resolved
	// address is a Tailscale one, and exempts Tailscale IPv6 from the fc00::/7
	// block (byoHermes.allowTailnetHttp, on by default in Swift).
	AllowTailnetHTTP bool
	// Resolver defaults to SystemResolver. Tests inject a stub.
	Resolver Resolver
}

// SystemResolver is SystemHostResolver: IP literals short-circuit, and any
// lookup failure is Unresolvable.
func SystemResolver(ctx context.Context, host string) ([]string, error) {
	if parseIPv4(host) != nil || strings.Contains(host, ":") {
		return []string{host}, nil
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, &Rejection{Unresolvable, host}
	}
	return addrs, nil
}

// Validate checks the URL syntactically, resolves its host and classifies every
// address. Errors are *Rejection, except what a custom Resolver returns.
//
// Go's URL parser is not Foundation's: the two can disagree on a malformed URL
// (Go rejects "10.0.0.1:8080" as invalidURL; Swift sees no scheme). Both refuse
// it, so only the wording of the rejection differs.
func (g Guard) Validate(ctx context.Context, rawURL string) (*url.URL, error) {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(rawURL)
	if trimmed == "" {
		return nil, &Rejection{InvalidURL, ""}
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, &Rejection{InvalidURL, ""}
	}
	scheme := strings.ToLower(u.Scheme)
	// http under RequireHTTPS isn't rejected yet: the tailnet carve-out needs
	// the resolved addresses first.
	httpPendingTailnet := false
	switch scheme {
	case "https":
	case "http":
		if g.RequireHTTPS {
			if !g.AllowTailnetHTTP {
				return nil, &Rejection{SchemeNotAllowed, scheme}
			}
			httpPendingTailnet = true
		}
	default:
		return nil, &Rejection{SchemeNotAllowed, scheme}
	}
	host := u.Hostname()
	if host == "" {
		return nil, &Rejection{HostMissing, ""}
	}
	if isHostnameLoopbackLiteral(host) {
		// Loopback is never a tailnet address, so the deferred http rejection
		// fires even when private ranges are open.
		if httpPendingTailnet {
			return nil, &Rejection{SchemeNotAllowed, scheme}
		}
		if g.AllowPrivateRanges {
			return u, nil
		}
		return nil, &Rejection{Loopback, host}
	}

	resolve := g.Resolver
	if resolve == nil {
		resolve = SystemResolver
	}
	addresses, err := resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, &Rejection{Unresolvable, host}
	}
	if httpPendingTailnet && slices.ContainsFunc(addresses, func(a string) bool { return !IsTailscale(a) }) {
		return nil, &Rejection{SchemeNotAllowed, scheme}
	}
	for _, ip := range addresses {
		if rej := g.classify(ip); rej != nil {
			return nil, rej
		}
	}
	return u, nil
}

func (g Guard) classify(ip string) *Rejection {
	if IsLoopback(ip) {
		if g.AllowPrivateRanges {
			return nil
		}
		return &Rejection{Loopback, ip}
	}
	if IsLinkLocal(ip) {
		if g.AllowPrivateRanges {
			return nil
		}
		return &Rejection{LinkLocal, ip}
	}
	// Tailscale IPv6 sits inside fc00::/7, so this runs before the unique-local
	// block, or MagicDNS AAAA answers would fail even over https.
	if IsTailscale(ip) && g.AllowTailnetHTTP {
		return nil
	}
	if IsPrivate(ip) {
		if g.AllowPrivateRanges {
			return nil
		}
		return &Rejection{PrivateAddress, ip}
	}
	return nil
}

func isHostnameLoopbackLiteral(host string) bool {
	lower := strings.ToLower(host)
	if lower == "localhost" || lower == "ip6-localhost" || lower == "ip6-loopback" {
		return true
	}
	return IsLoopback(strings.Trim(lower, "[]"))
}

// IsLoopback matches 127.0.0.0/8 and the two spellings of ::1 Swift checks.
func IsLoopback(ip string) bool {
	if ip == "::1" || ip == "0:0:0:0:0:0:0:1" {
		return true
	}
	v4 := parseIPv4(ip)
	return v4 != nil && v4[0] == 127
}

// IsLinkLocal matches 169.254.0.0/16 (covers EC2 metadata) and fe80:.
func IsLinkLocal(ip string) bool {
	if v4 := parseIPv4(ip); v4 != nil && v4[0] == 169 && v4[1] == 254 {
		return true
	}
	return strings.HasPrefix(strings.ToLower(ip), "fe80:")
}

// IsTailscale matches 100.64.0.0/10 and fd7a:115c:a1e0::/48.
func IsTailscale(ip string) bool {
	if v4 := parseIPv4(ip); v4 != nil {
		return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
	}
	return strings.HasPrefix(strings.ToLower(ip), "fd7a:115c:a1e0:")
}

// IsPrivate matches RFC1918, 0.0.0.0/8, fc00::/7 and IPv4-mapped forms of any
// private, loopback or link-local address.
func IsPrivate(ip string) bool {
	if v4 := parseIPv4(ip); v4 != nil {
		if v4[0] == 10 || (v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31) || (v4[0] == 192 && v4[1] == 168) || v4[0] == 0 {
			return true
		}
	}
	lower := strings.ToLower(ip)
	if strings.HasPrefix(lower, "fc") || strings.HasPrefix(lower, "fd") {
		return true
	}
	if tail, ok := strings.CutPrefix(lower, "::ffff:"); ok {
		return IsPrivate(tail) || IsLoopback(tail) || IsLinkLocal(tail)
	}
	return false
}

// parseIPv4 is Swift's parseIPv4: four dot-separated Ints in 0...255. Like
// Int("+1"), Atoi accepts a sign, and like split(separator:), empty parts are
// dropped.
func parseIPv4(ip string) []int {
	parts := strings.FieldsFunc(ip, func(r rune) bool { return r == '.' })
	if len(parts) != 4 {
		return nil
	}
	nums := make([]int, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return nil
		}
		nums[i] = n
	}
	return nums
}
