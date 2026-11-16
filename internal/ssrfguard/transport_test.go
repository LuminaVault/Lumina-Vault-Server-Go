package ssrfguard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

func TestCheckAddr(t *testing.T) {
	cases := []struct {
		ip   string
		kind Kind // "" means allowed
	}{
		{"8.8.8.8", ""},
		{"2606:4700:4700::1111", ""},
		{"127.0.0.1", Loopback},
		{"127.255.0.9", Loopback},
		{"::1", Loopback},
		{"::ffff:127.0.0.1", Loopback},
		{"169.254.169.254", LinkLocal},
		{"fe80::1", LinkLocal},
		{"10.1.2.3", PrivateAddress},
		{"172.16.0.1", PrivateAddress},
		{"192.168.1.1", PrivateAddress},
		{"::ffff:10.0.0.1", PrivateAddress},
		{"fd00::1", PrivateAddress},
		{"0.0.0.0", PrivateAddress},
		{"0.1.2.3", PrivateAddress},
		{"::", PrivateAddress},
		{"239.1.2.3", PrivateAddress},
		{"224.0.0.1", LinkLocal},
		{"100.64.0.1", PrivateAddress},
		{"100.101.102.103", PrivateAddress},
		{"fd7a:115c:a1e0::1", PrivateAddress},
	}
	for _, c := range cases {
		err := Guard{}.CheckAddr(netip.MustParseAddr(c.ip))
		if c.kind == "" {
			if err != nil {
				t.Errorf("%s: got %v, want allowed", c.ip, err)
			}
			continue
		}
		var rej *Rejection
		if !errors.As(err, &rej) || rej.Kind != c.kind {
			t.Errorf("%s: got %v, want %s", c.ip, err, c.kind)
		}
	}
}

func TestCheckAddrTailnetAndPrivateSwitches(t *testing.T) {
	tailnet := Guard{AllowTailnetHTTP: true}
	for _, ip := range []string{"100.101.102.103", "fd7a:115c:a1e0::1"} {
		if err := tailnet.CheckAddr(netip.MustParseAddr(ip)); err != nil {
			t.Errorf("tailnet %s: %v", ip, err)
		}
	}
	if err := tailnet.CheckAddr(netip.MustParseAddr("10.0.0.1")); err == nil {
		t.Error("tailnet guard allowed 10.0.0.1")
	}
	open := Guard{AllowPrivateRanges: true}
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254"} {
		if err := open.CheckAddr(netip.MustParseAddr(ip)); err != nil {
			t.Errorf("open %s: %v", ip, err)
		}
	}
}

func TestClientRefusesLoopbackAtDial(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	defer srv.Close()

	err := get(Guard{}.Client(5*time.Second), srv.URL)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != Loopback {
		t.Fatalf("got %v, want a loopback rejection", err)
	}
	if hits != 0 {
		t.Fatalf("server saw %d requests", hits)
	}

	resp, err := Guard{AllowPrivateRanges: true}.Client(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("open guard: %v", err)
	}
	_ = resp.Body.Close()
}

// A name is checked by what it resolves to, so "localhost" is refused at dial
// even though no URL check ran.
func TestClientRefusesNameResolvingToLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	port := netip.MustParseAddrPort(srv.Listener.Addr().String()).Port()
	u := "http://localhost:" + strconv.Itoa(int(port))

	err := get(Guard{}.Client(5*time.Second), u)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != Loopback {
		t.Fatalf("got %v, want a loopback rejection", err)
	}
}

// A redirect is a new dial, so a hop to a refused address fails even when the
// first server was allowed. Here the first server is reached through an
// allow-list guard that admits only its own port.
func TestClientChecksEveryRedirectHop(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL, http.StatusFound)
	}))
	defer outer.Close()

	outerAddr := netip.MustParseAddrPort(outer.Listener.Addr().String())
	client := &http.Client{Transport: &portGate{
		allow: outerAddr, rt: Guard{AllowPrivateRanges: true}.Transport(), strict: Guard{}.Transport(),
	}}

	err := get(client, outer.URL)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != Loopback {
		t.Fatalf("got %v, want the redirect hop refused", err)
	}
}

// portGate sends requests for one host:port through an open transport and
// everything else through the strict one, standing in for "a public server".
type portGate struct {
	allow  netip.AddrPort
	rt     http.RoundTripper
	strict http.RoundTripper
}

func (p *portGate) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == p.allow.String() {
		return p.rt.RoundTrip(r)
	}
	return p.strict.RoundTrip(r)
}

// get is client.Get with the body closed, for tests that only want the error.
func get(client *http.Client, u string) error {
	resp, err := client.Get(u)
	if resp != nil {
		_ = resp.Body.Close()
	}
	return err
}
