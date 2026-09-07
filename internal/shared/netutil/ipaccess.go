package netutil

import (
	"fmt"
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
func NewIPAccessChecker(allowCIDRs, denyIPs []string) (*IPAccessChecker, error) {
	allowNets, denyNets, err := parseIPAccessRules(allowCIDRs, denyIPs)
	if err != nil {
		return nil, err
	}

	return &IPAccessChecker{
		allowNets: allowNets,
		denyNets:  denyNets,
		hasAllow:  len(allowNets) > 0,
		hasDeny:   len(denyNets) > 0,
	}, nil
}

// ValidateIPAccessRules verifies that all allow/deny entries are valid IPs or CIDR ranges.
func ValidateIPAccessRules(allowCIDRs, denyIPs []string) error {
	_, _, err := parseIPAccessRules(allowCIDRs, denyIPs)
	return err
}

func parseIPAccessRules(allowCIDRs, denyIPs []string) ([]*net.IPNet, []*net.IPNet, error) {
	allowNets, err := parseIPAccessEntries("allow IP/CIDR", allowCIDRs)
	if err != nil {
		return nil, nil, err
	}

	denyNets, err := parseIPAccessEntries("deny IP/CIDR", denyIPs)
	if err != nil {
		return nil, nil, err
	}

	return allowNets, denyNets, nil
}

func parseIPAccessEntries(label string, entries []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(entries))

	for i, entry := range entries {
		normalized, err := normalizeIPAccessEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("%s entry %d (%q): %w", label, i+1, entry, err)
		}

		_, ipNet, err := net.ParseCIDR(normalized)
		if err != nil {
			return nil, fmt.Errorf("%s entry %d (%q): %w", label, i+1, entry, err)
		}
		nets = append(nets, ipNet)
	}

	return nets, nil
}

func normalizeIPAccessEntry(entry string) (string, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", fmt.Errorf("must not be empty")
	}

	if strings.Contains(entry, "/") {
		return entry, nil
	}

	ip := net.ParseIP(entry)
	if ip == nil {
		return "", fmt.Errorf("must be an IP address or CIDR range")
	}

	if ip.To4() != nil {
		return entry + "/32", nil
	}
	return entry + "/128", nil
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
