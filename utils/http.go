package utils

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the client IP for a request.
//
// When trustProxyHeaders is false (default), only RemoteAddr is used. This is
// safe when pooml is exposed directly to clients.
//
// When true, the rightmost entry of X-Forwarded-For is used (the IP the proxy
// adds for the connecting client), falling back to RemoteAddr if the header is
// absent or malformed. Set POOML_TRUST_PROXY_HEADERS=true ONLY when pooml is
// behind a reverse proxy that strips or replaces incoming X-Forwarded-For from
// clients - otherwise attackers can spoof their IP and bypass throttling.
// Assumes a single proxy hop; multi-hop deployments should canonicalize the
// header at the edge proxy before it reaches pooml.
//
// Header.Values (not Get) is used so a proxy that emits its own separate
// X-Forwarded-For line rather than appending to the client's still yields the
// proxy-added value as the rightmost entry - Get returns only the first line.
func ClientIP(req *http.Request, trustProxyHeaders bool) string {
	if trustProxyHeaders {
		var parts []string
		for _, line := range req.Header.Values("X-Forwarded-For") {
			parts = append(parts, strings.Split(line, ",")...)
		}
		for i := len(parts) - 1; i >= 0; i-- {
			if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}
