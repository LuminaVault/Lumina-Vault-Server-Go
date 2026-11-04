package ssrfguard

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// stub pins DNS answers and records lookups, so no test touches the network.
type stub struct {
	answers  map[string][]string
	resolved []string
}

func (s *stub) resolve(_ context.Context, host string) ([]string, error) {
	s.resolved = append(s.resolved, host)
	return s.answers[host], nil
}

func answers(host string, ips ...string) *stub {
	return &stub{answers: map[string][]string{host: ips}}
}

// TestRejections ports Swift's SSRFGuardTests, one row per case.
func TestRejections(t *testing.T) {
	const ts = "hermes-vps.tail562587.ts.net"
	cases := []struct {
		name                    string
		private, https, tailnet bool
		stub                    *stub
		url                     string
		want                    string // rejection; empty means accepted
	}{
		{name: "rejects ftp scheme", stub: answers("example.com", "93.184.216.34"), url: "ftp://example.com", want: `schemeNotAllowed("ftp")`},
		{name: "rejects http when requireHTTPS", https: true, stub: answers("example.com", "93.184.216.34"), url: "http://example.com", want: `schemeNotAllowed("http")`},
		{name: "allows http to public host when not requiring https", stub: answers("example.com", "93.184.216.34"), url: "http://example.com"},
		{name: "allows bare public IP over http", stub: answers("203.0.113.5", "203.0.113.5"), url: "http://203.0.113.5:8080"},
		{name: "still rejects private IP over http", stub: answers("evil.com", "10.0.0.5"), url: "http://evil.com", want: `privateAddress("10.0.0.5")`},
		{name: "rejects literal localhost", stub: &stub{}, url: "http://localhost", want: `loopback("localhost")`},
		{name: "rejects literal 127.0.0.1", stub: answers("127.0.0.1", "127.0.0.1"), url: "http://127.0.0.1", want: `loopback("127.0.0.1")`},
		{name: "rejects RFC1918 10/8", stub: answers("evil.com", "10.0.0.5"), url: "http://evil.com", want: `privateAddress("10.0.0.5")`},
		{name: "rejects RFC1918 192.168", stub: answers("evil.com", "192.168.1.1"), url: "http://evil.com", want: `privateAddress("192.168.1.1")`},
		{name: "rejects RFC1918 172.16-31", stub: answers("evil.com", "172.20.0.1"), url: "http://evil.com", want: `privateAddress("172.20.0.1")`},
		{name: "rejects EC2 metadata", stub: answers("evil.com", "169.254.169.254"), url: "http://evil.com", want: `linkLocal("169.254.169.254")`},
		{name: "rejects resolved IPv6 loopback", stub: answers("evil.com", "::1"), url: "http://evil.com", want: `loopback("::1")`},
		{name: "rejects resolved IPv6 unique-local", stub: answers("evil.com", "fc00::1"), url: "http://evil.com", want: `privateAddress("fc00::1")`},
		{name: "rejects 0.0.0.0", stub: answers("evil.com", "0.0.0.0"), url: "http://evil.com", want: `privateAddress("0.0.0.0")`},
		{name: "accepts public host", stub: answers("api.example.com", "93.184.216.34"), url: "https://api.example.com"},
		{name: "unresolvable", stub: &stub{}, url: "https://ghost.example", want: `unresolvable("ghost.example")`},
		{name: "allowPrivateRanges opens loopback", private: true, stub: answers("127.0.0.1", "127.0.0.1"), url: "http://127.0.0.1"},
		{name: "http to tailscale IPv4 despite requireHTTPS", https: true, tailnet: true, stub: answers(ts, "100.102.137.69"), url: "http://" + ts + ":8642"},
		{name: "MagicDNS dual-stack over http", https: true, tailnet: true, stub: answers(ts, "100.102.137.69", "fd7a:115c:a1e0::6f01:89b5"), url: "http://" + ts + ":8642"},
		{name: "MagicDNS dual-stack over https", https: true, tailnet: true, stub: answers(ts, "100.102.137.69", "fd7a:115c:a1e0::6f01:89b5"), url: "https://" + ts + ":8642"},
		{name: "http to public host with carve-out on", https: true, tailnet: true, stub: answers("example.com", "93.184.216.34"), url: "http://example.com", want: `schemeNotAllowed("http")`},
		{name: "http with mixed tailnet and public", https: true, tailnet: true, stub: answers("evil.com", "100.102.137.69", "93.184.216.34"), url: "http://evil.com", want: `schemeNotAllowed("http")`},
		{name: "carve-out disabled restores strict requireHTTPS", https: true, stub: answers(ts, "100.102.137.69"), url: "http://" + ts + ":8642", want: `schemeNotAllowed("http")`},
		{name: "carve-out disabled rejects tailscale IPv6", stub: answers(ts, "fd7a:115c:a1e0::6f01:89b5"), url: "https://" + ts, want: `privateAddress("fd7a:115c:a1e0::6f01:89b5")`},
		{name: "http to loopback under requireHTTPS with private ranges open", private: true, https: true, tailnet: true, stub: answers("127.0.0.1", "127.0.0.1"), url: "http://127.0.0.1", want: `schemeNotAllowed("http")`},
		{name: "IPv6 literal loopback", stub: &stub{}, url: "http://[::1]:8642", want: `loopback("::1")`},
		{name: "IPv4-mapped private", stub: answers("evil.com", "::ffff:10.0.0.1"), url: "http://evil.com", want: `privateAddress("::ffff:10.0.0.1")`},
		{name: "blank", stub: &stub{}, url: "   ", want: "invalidURL"},
		{name: "no scheme", stub: &stub{}, url: "example.com", want: `schemeNotAllowed("")`},
		{name: "no host", stub: &stub{}, url: "https://", want: "hostMissing"},
		{name: "scheme is case-insensitive", https: true, stub: answers("example.com", "93.184.216.34"), url: "HTTPS://example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := Guard{AllowPrivateRanges: tc.private, RequireHTTPS: tc.https, AllowTailnetHTTP: tc.tailnet, Resolver: tc.stub.resolve}
			u, err := g.Validate(context.Background(), tc.url)
			if tc.want == "" {
				require.NoError(t, err)
				require.NotNil(t, u)
				return
			}
			require.EqualError(t, err, tc.want)
		})
	}
}

// Swift "re-resolves on every call (DNS rebinding defense)".
func TestReResolvesOnEveryCall(t *testing.T) {
	s := answers("evil.com", "93.184.216.34")
	g := Guard{Resolver: s.resolve}
	ctx := context.Background()
	_, err := g.Validate(ctx, "https://evil.com")
	require.NoError(t, err)
	s.answers["evil.com"] = []string{"127.0.0.1"}
	_, err = g.Validate(ctx, "https://evil.com")
	require.EqualError(t, err, `loopback("127.0.0.1")`)
	require.Equal(t, []string{"evil.com", "evil.com"}, s.resolved)
}

// Swift "rebinding away from tailnet re-rejects http on next call".
func TestRebindingAwayFromTailnet(t *testing.T) {
	s := answers("ts-name.ts.net", "100.100.1.1")
	g := Guard{RequireHTTPS: true, AllowTailnetHTTP: true, Resolver: s.resolve}
	ctx := context.Background()
	_, err := g.Validate(ctx, "http://ts-name.ts.net")
	require.NoError(t, err)
	s.answers["ts-name.ts.net"] = []string{"93.184.216.34"}
	_, err = g.Validate(ctx, "http://ts-name.ts.net")
	require.EqualError(t, err, `schemeNotAllowed("http")`)
}

func TestIsTailscale(t *testing.T) {
	for _, ip := range []string{"100.64.0.1", "100.102.137.69", "100.127.255.255", "fd7a:115c:a1e0::6f01:89b5"} {
		require.True(t, IsTailscale(ip), ip)
	}
	for _, ip := range []string{"100.63.255.255", "100.128.0.1", "fd00::1", "93.184.216.34"} {
		require.False(t, IsTailscale(ip), ip)
	}
}

func TestParseIPv4(t *testing.T) {
	require.Equal(t, []int{192, 168, 1, 1}, parseIPv4("192.168.1.1"))
	require.Nil(t, parseIPv4("abc.def.ghi.jkl"))
	require.Nil(t, parseIPv4("999.0.0.1"))
	require.Nil(t, parseIPv4("1.2.3"))
}

func TestClassifiers(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "127.8.8.8", "::1", "0:0:0:0:0:0:0:1"} {
		require.True(t, IsLoopback(ip), ip)
	}
	require.False(t, IsLoopback("128.0.0.1"))
	for _, ip := range []string{"169.254.0.1", "fe80::1", "FE80::1"} {
		require.True(t, IsLinkLocal(ip), ip)
	}
	require.False(t, IsLinkLocal("169.253.0.1"))
	for _, ip := range []string{"10.1.1.1", "172.16.0.1", "172.31.0.1", "192.168.0.1", "0.1.2.3", "fc00::1", "fd00::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.1.1"} {
		require.True(t, IsPrivate(ip), ip)
	}
	for _, ip := range []string{"172.32.0.1", "8.8.8.8", "::ffff:8.8.8.8", "2001:db8::1"} {
		require.False(t, IsPrivate(ip), ip)
	}
}

func TestSystemResolverShortCircuitsLiterals(t *testing.T) {
	for _, host := range []string{"203.0.113.5", "2001:db8::1"} {
		got, err := SystemResolver(context.Background(), host)
		require.NoError(t, err)
		require.Equal(t, []string{host}, got)
	}
}
