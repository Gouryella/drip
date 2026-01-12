package netutil

import (
	"net"
	"strings"
)

// IPAccessChecker checks if an IP address is allowed based on whitelist/blacklist rules.
type IPAccessChecker struct {
	allowNets []*net.IPNet // Allowed CIDR ranges (whitelist)
	denyNets  []*net.IPNet // Denied CIDR ranges (blacklist)
	hasAllow  bool         // Whether whitelist is configured
	hasDeny   bool         // Whether blacklist is configured
}

// NewIPAccessChecker creates a new IP access checker from CIDR and IP lists.
// allowCIDRs: list of CIDR ranges to allow (e.g., "192.168.1.0/24", "10.0.0.0/8")
// denyIPs: list of CIDR ranges or IP addresses to deny (e.g., "192.168.0.0/16", "1.2.3.4")
func NewIPAccessChecker(allowCIDRs, denyIPs []string) *IPAccessChecker {
	checker := &IPAccessChecker{}

	// Parse allowed CIDRs
	for _, cidr := range allowCIDRs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}

		// If no "/" in the string, treat it as a single IP (/32 for IPv4, /128 for IPv6)
		if !strings.Contains(cidr, "/") {
			ip := net.ParseIP(cidr)
			if ip != nil {
				if ip.To4() != nil {
					cidr = cidr + "/32"
				} else {
					cidr = cidr + "/128"
				}
			}
		}

		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		checker.allowNets = append(checker.allowNets, ipNet)
	}
	checker.hasAllow = len(checker.allowNets) > 0

	// Parse denied IPs/CIDRs
	for _, ipStr := range denyIPs {
		ipStr = strings.TrimSpace(ipStr)
		if ipStr == "" {
			continue
		}

		// If no "/" in the string, treat it as a single IP (/32 for IPv4, /128 for IPv6)
		if !strings.Contains(ipStr, "/") {
			ip := net.ParseIP(ipStr)
			if ip != nil {
				if ip.To4() != nil {
					ipStr = ipStr + "/32"
				} else {
					ipStr = ipStr + "/128"
				}
			}
		}

		_, ipNet, err := net.ParseCIDR(ipStr)
		if err != nil {
			continue
		}
		checker.denyNets = append(checker.denyNets, ipNet)
	}
	checker.hasDeny = len(checker.denyNets) > 0

	return checker
}

// IsAllowed checks if the given IP address is allowed.
// Rules:
// 1. If IP is in deny list, reject
// 2. If whitelist is configured and IP is not in whitelist, reject
// 3. Otherwise, allow
func (c *IPAccessChecker) IsAllowed(ipStr string) bool {
	if c == nil || (!c.hasAllow && !c.hasDeny) {
		return true // No rules configured, allow all
	}

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false // Invalid IP, reject
	}

	// Check deny list first (blacklist takes priority)
	if c.hasDeny {
		for _, denyNet := range c.denyNets {
			if denyNet.Contains(ip) {
				return false
			}
		}
	}

	// Check allow list (whitelist)
	if c.hasAllow {
		for _, allowNet := range c.allowNets {
			if allowNet.Contains(ip) {
				return true
			}
		}
		return false // Whitelist configured but IP not in it
	}

	return true // No whitelist, and not in blacklist
}

// HasRules returns true if any access control rules are configured.
func (c *IPAccessChecker) HasRules() bool {
	return c != nil && (c.hasAllow || c.hasDeny)
}

// ExtractIP extracts the IP address from a remote address string (e.g., "192.168.1.1:12345").
func ExtractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// Maybe it's just an IP without port
		if ip := net.ParseIP(remoteAddr); ip != nil {
			return remoteAddr
		}
		return ""
	}
	return host
}
