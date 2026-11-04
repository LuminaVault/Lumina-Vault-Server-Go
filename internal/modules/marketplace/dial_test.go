package marketplace_test

import (
	"context"
	"net"
)

// dialTo sends every connection to addr, whatever host the URL names.
func dialTo(addr string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
}
