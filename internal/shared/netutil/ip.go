package netutil

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

var privateNetworks []*net.IPNet

func init() {
	privateCIDRs := []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}
	for _, cidr := range privateCIDRs {
		_, ipNet, _ := net.ParseCIDR(cidr)
		privateNetworks = append(privateNetworks, ipNet)
	}
}

// TrustedProxySet contains the proxy IP ranges allowed to supply forwarding headers.
type TrustedProxySet struct {
	networks []*net.IPNet
}

// NewTrustedProxySet parses trusted proxy CIDRs and individual IP addresses.
func NewTrustedProxySet(entries []string) (*TrustedProxySet, error) {
	set := &TrustedProxySet{}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		cidr := entry
		if !strings.Contains(cidr, "/") {
			ip := net.ParseIP(cidr)
			if ip == nil {
				return nil, fmt.Errorf("invalid trusted proxy address %q", entry)
			}
			if ip.To4() != nil {
				cidr += "/32"
			} else {
				cidr += "/128"
			}
		}

		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", entry, err)
		}
		set.networks = append(set.networks, network)
	}

	return set, nil
}

// IsEmpty reports whether the trusted proxy set has no configured networks.
func (s *TrustedProxySet) IsEmpty() bool {
	return s == nil || len(s.networks) == 0
}

// ContainsIP reports whether ip is in the configured trusted proxy ranges.
func (s *TrustedProxySet) ContainsIP(ip net.IP) bool {
	if s == nil || ip == nil {
		return false
	}
	for _, network := range s.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// Contains reports whether ipStr parses as an IP in the configured trusted proxy ranges.
func (s *TrustedProxySet) Contains(ipStr string) bool {
	return s.ContainsIP(net.ParseIP(ipStr))
}

// ExtractRemoteIP extracts the IP address from a remote address string (host:port format).
func ExtractRemoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// IsPrivateIP checks if the given IP is a private/loopback address.
func IsPrivateIP(ip string) bool {
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false
	}

	for _, network := range privateNetworks {
		if network.Contains(parsedIP) {
			return true
		}
	}

	return false
}

// ExtractClientIP extracts the direct client IP from the request.
// Forwarding headers are ignored unless ExtractClientIPWithTrustedProxies is
// called with an explicit trusted proxy set.
func ExtractClientIP(r *http.Request) string {
	return ExtractClientIPWithTrustedProxies(r, nil)
}

// ExtractClientIPWithTrustedProxies extracts the client IP from a request,
// trusting X-Forwarded-For and X-Real-IP only when the direct peer is an
// explicitly configured trusted proxy.
func ExtractClientIPWithTrustedProxies(r *http.Request, trustedProxies *TrustedProxySet) string {
	remoteIP := ExtractRemoteIP(r.RemoteAddr)
	if trustedProxies.IsEmpty() || !trustedProxies.Contains(remoteIP) {
		return remoteIP
	}

	if clientIP := extractForwardedForClientIP(r.Header.Get("X-Forwarded-For"), trustedProxies); clientIP != "" {
		return clientIP
	}

	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		ip := strings.TrimSpace(xri)
		parsedIP := net.ParseIP(ip)
		if parsedIP != nil {
			return parsedIP.String()
		}
	}

	return remoteIP
}

func extractForwardedForClientIP(xff string, trustedProxies *TrustedProxySet) string {
	if xff == "" {
		return ""
	}

	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			continue
		}
		if !trustedProxies.ContainsIP(ip) {
			return ip.String()
		}
	}

	return ""
}
