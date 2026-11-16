package ssrfguard

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// CheckAddr reports whether the guard allows a connection to ip. It is the
// dial-time half of the guard: Transport calls it on the address the socket is
// about to connect to, after DNS, so a name that resolves (or re-resolves) to a
// private address is refused, and so is every hop of a redirect chain.
//
// It is stricter than Validate, which keeps Swift's string checks for its wire
// contract. Here the address is parsed, IPv4-mapped IPv6 is unmapped, and
// unspecified, multicast and the shared 100.64.0.0/10 range are refused too.
// AllowPrivateRanges opens everything; AllowTailnetHTTP opens Tailscale ranges.
func (g Guard) CheckAddr(ip netip.Addr) error {
	if g.AllowPrivateRanges {
		return nil
	}
	ip = ip.Unmap()
	s := ip.String()
	switch {
	case !ip.IsValid():
		return &Rejection{InvalidURL, ""}
	case ip.IsLoopback():
		return &Rejection{Loopback, s}
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return &Rejection{LinkLocal, s}
	case g.AllowTailnetHTTP && IsTailscale(s):
		return nil
	case ip.IsPrivate(), ip.IsUnspecified(), ip.IsMulticast(), ip.IsInterfaceLocalMulticast(),
		sharedAddressSpace.Contains(ip), ip.Is4() && ip.As4()[0] == 0:
		return &Rejection{PrivateAddress, s}
	}
	return nil
}

// sharedAddressSpace is RFC 6598, which Tailscale and carrier NAT use.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// control runs on every socket before connect.
func (g Guard) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("ssrfguard: unparseable dial address %q: %w", address, err)
	}
	return g.CheckAddr(ap.Addr())
}

// Transport is an http.Transport for requests to URLs a user supplied. Its
// dialer refuses any address CheckAddr refuses, and it never uses a proxy
// (a proxy would dial on our behalf and bypass the check).
//
// Use it for the user-controlled case only. Clients that reach in-cluster
// services (Hermes, whisper) on purpose must not use it.
func (g Guard) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: g.control}
	t.DialContext = dialer.DialContext
	return t
}

// Client is an http.Client over Transport. It follows redirects, which is safe
// because each hop dials through the same check. A zero timeout means none.
func (g Guard) Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: g.Transport(), Timeout: timeout}
}
