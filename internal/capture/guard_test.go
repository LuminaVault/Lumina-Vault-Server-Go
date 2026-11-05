package capture

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func mustURL(t *testing.T, raw string) *URL {
	t.Helper()
	u, ok := ParseURL(raw)
	require.True(t, ok, raw)
	return u
}

// URLEnricherGuardTests.swift.
func TestPrivateAndNonHTTPRejected(t *testing.T) {
	for _, raw := range []string{
		"http://localhost/admin", "http://127.0.0.1:5432", "http://10.0.0.1/", "http://192.168.1.10/api",
		"http://169.254.169.254/latest/meta-data/", "http://172.16.0.1/", "http://172.31.255.255/", "http://0.0.0.0/",
		"http://[::1]/", "http://[fc00::1]/", "http://[fe80::1]/", "http://metadata.google.internal/",
		"http://server.internal/", "http://printer.local/", "ftp://example.com/", "file:///etc/passwd", "javascript:alert(1)",
	} {
		require.False(t, IsPublic(mustURL(t, raw)), raw)
	}
}

func TestPublicHostsPass(t *testing.T) {
	for _, raw := range []string{
		"https://youtube.com/watch?v=abc", "https://www.youtube.com/watch?v=abc", "https://youtu.be/abc",
		"https://x.com/jack/status/20", "https://twitter.com/jack/status/20", "https://example.com/page",
		"http://172.32.0.1/", "http://172.15.0.1/",
	} {
		require.True(t, IsPublic(mustURL(t, raw)), raw)
	}
}

func TestYouTubeRoutesOnlyYouTubeHosts(t *testing.T) {
	yt := YouTubeEnricher{}
	require.True(t, yt.CanHandle(mustURL(t, "https://www.youtube.com/watch?v=x")))
	require.True(t, yt.CanHandle(mustURL(t, "https://youtu.be/x")))
	require.False(t, yt.CanHandle(mustURL(t, "https://example.com")))
}

func TestXRoutesTwitterAndXHosts(t *testing.T) {
	x := XEnricher{}
	require.True(t, x.CanHandle(mustURL(t, "https://twitter.com/u/status/1")))
	require.True(t, x.CanHandle(mustURL(t, "https://x.com/u/status/1")))
	require.False(t, x.CanHandle(mustURL(t, "https://example.com")))
}

func TestGenericOGIsTheCatchAll(t *testing.T) {
	og := GenericOGEnricher{}
	require.True(t, og.CanHandle(mustURL(t, "https://example.com")))
	require.True(t, og.CanHandle(mustURL(t, "https://github.com")))
}

// Not in Swift's tests: what the guard gets wrong, kept on purpose. The IPv6
// prefix checks run on every host, labels that aren't numbers are dropped, and
// other spellings of a private address pass. Whether to replace it with
// internal/ssrfguard is an owner decision.
func TestGuardQuirks(t *testing.T) {
	for _, raw := range []string{"https://fcbarcelona.com/", "https://fdroid.org/", "https://fe80.example/", "http://10.0.0.1.example.com/"} {
		require.False(t, IsPublic(mustURL(t, raw)), raw)
	}
	for _, raw := range []string{"http://127.1/", "http://2130706433/", "http://0x7f.0.0.1/", "http://[::ffff:127.0.0.1]/"} {
		require.True(t, IsPublic(mustURL(t, raw)), raw)
	}
}

// XEnricher's "contains x.com" takes any host ending in it. Swift's order: YouTube,
// X, then generic OG.
func TestXMatchesNetflix(t *testing.T) {
	require.True(t, XEnricher{}.CanHandle(mustURL(t, "https://netflix.com/title/1")))
}
