// Package httpguard contains request limits for the loopback gateway listener.
package httpguard

import (
	"net"
	"net/http"
	"net/netip"
)

// ClientIP accepts one X-Real-IP only from the local reverse proxy. Both
// supported proxy configurations overwrite that header with the socket peer's
// address. Forwarded and X-Forwarded-For are deliberately never consulted.
// A direct local caller has the same trust as the proxy's service account.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	peer = peer.Unmap()
	if peer.IsLoopback() {
		values := r.Header.Values("X-Real-IP")
		if len(values) == 1 {
			if client, err := netip.ParseAddr(values[0]); err == nil && client.Zone() == "" {
				return client.Unmap().String()
			}
		}
	}
	return peer.String()
}
