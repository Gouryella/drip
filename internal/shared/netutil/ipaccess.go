package netutil

import (
	"fmt"
	"net"
	"strings"
)

// ipAccessRule is a CIDR rule whose network address and mask are precomputed
// at construction time, so matching does not allocate.
type ipAccessRule struct {
	network *net.IPNet
	addr    []byte
	mask    []byte
}

func newIPAccessRule(network *net.IPNet) ipAccessRule {
	return ipAccessRule{
		network: network,
		addr:    network.IP,
		mask:    network.Mask,
	}
}

// contains reports whether ip belongs to the rule. The common IPv4/IPv6 cases
// use a manual masked comparison; mismatched address families fall back to
// net.IPNet.Contains.
func (r *ipAccessRule) contains(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		if len(r.addr) == net.IPv4len {
			return matchMaskedIP(v4, r.addr, r.mask)
		}
		return r.network.Contains(ip)
	}
	if len(ip) == net.IPv6len && len(r.addr) == net.IPv6len {
		return matchMaskedIP(ip, r.addr, r.mask)
	}
	return r.network.Contains(ip)
}

func matchMaskedIP(ip, network, mask []byte) bool {
	switch len(network) {
	case net.IPv4len:
		if len(ip) != net.IPv4len || len(mask) != net.IPv4len {
			return false
		}
		return ip[0]&mask[0] == network[0] &&
			ip[1]&mask[1] == network[1] &&
			ip[2]&mask[2] == network[2] &&
			ip[3]&mask[3] == network[3]
	case net.IPv6len:
		if len(ip) != net.IPv6len || len(mask) != net.IPv6len {
			return false
		}
		for i := 0; i < net.IPv6len; i++ {
			if ip[i]&mask[i] != network[i] {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// IPAccessChecker checks if an IP address is allowed based on whitelist/blacklist rules.
type IPAccessChecker struct {
	allow    []ipAccessRule // Allowed CIDR ranges (whitelist)
	deny     []ipAccessRule // Denied CIDR ranges (blacklist)
	hasAllow bool           // Whether whitelist is configured
	hasDeny  bool           // Whether blacklist is configured
}

// NewIPAccessChecker creates a new IP access checker from CIDR and IP lists.
// allowCIDRs: list of CIDR ranges to allow (e.g., "192.168.1.0/24", "10.0.0.0/8")
// denyIPs: list of CIDR ranges or IP addresses to deny (e.g., "192.168.0.0/16", "1.2.3.4")
func NewIPAccessChecker(allowCIDRs, denyIPs []string) (*IPAccessChecker, error) {
	allowRules, denyRules, err := parseIPAccessRules(allowCIDRs, denyIPs)
	if err != nil {
		return nil, err
	}

	return &IPAccessChecker{
		allow:    allowRules,
		deny:     denyRules,
		hasAllow: len(allowRules) > 0,
		hasDeny:  len(denyRules) > 0,
	}, nil
}

// ValidateIPAccessRules verifies that all allow/deny entries are valid IPs or CIDR ranges.
func ValidateIPAccessRules(allowCIDRs, denyIPs []string) error {
	_, _, err := parseIPAccessRules(allowCIDRs, denyIPs)
	return err
}

func parseIPAccessRules(allowCIDRs, denyIPs []string) ([]ipAccessRule, []ipAccessRule, error) {
	allowRules, err := parseIPAccessEntries("allow IP/CIDR", allowCIDRs)
	if err != nil {
		return nil, nil, err
	}

	denyRules, err := parseIPAccessEntries("deny IP/CIDR", denyIPs)
	if err != nil {
		return nil, nil, err
	}

	return allowRules, denyRules, nil
}

func parseIPAccessEntries(label string, entries []string) ([]ipAccessRule, error) {
	rules := make([]ipAccessRule, 0, len(entries))

	for i, entry := range entries {
		normalized, err := normalizeIPAccessEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("%s entry %d (%q): %w", label, i+1, entry, err)
		}

		_, ipNet, err := net.ParseCIDR(normalized)
		if err != nil {
			return nil, fmt.Errorf("%s entry %d (%q): %w", label, i+1, entry, err)
		}
		rules = append(rules, newIPAccessRule(ipNet))
	}

	return rules, nil
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
	return c.IsAllowedIP(net.ParseIP(ipStr))
}

// IsAllowedIP is IsAllowed for an already-parsed address.
func (c *IPAccessChecker) IsAllowedIP(ip net.IP) bool {
	if c == nil || (!c.hasAllow && !c.hasDeny) {
		return true // No rules configured, allow all
	}
	if ip == nil {
		return false // Invalid IP, reject
	}

	// Check deny list first (blacklist takes priority)
	if c.hasDeny {
		for i := range c.deny {
			if c.deny[i].contains(ip) {
				return false
			}
		}
	}

	// Check allow list (whitelist)
	if c.hasAllow {
		for i := range c.allow {
			if c.allow[i].contains(ip) {
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
