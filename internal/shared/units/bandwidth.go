// Package units parses the human-readable sizes used in Drip configuration.
package units

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseBandwidth converts a bandwidth string such as "1M", "500K" or "1G" into
// bytes per second. An empty string means no limit and yields 0.
//
// Suffixes are binary: 1K is 1024 bytes, 1M is 1024*1024, 1G is 1024*1024*1024.
func ParseBandwidth(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}

	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, nil
	}

	var multiplier int64 = 1
	switch {
	case strings.HasSuffix(s, "GB") || strings.HasSuffix(s, "G"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(strings.TrimSuffix(s, "GB"), "G")
	case strings.HasSuffix(s, "MB") || strings.HasSuffix(s, "M"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(strings.TrimSuffix(s, "MB"), "M")
	case strings.HasSuffix(s, "KB") || strings.HasSuffix(s, "K"):
		multiplier = 1024
		s = strings.TrimSuffix(strings.TrimSuffix(s, "KB"), "K")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}

	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil || val < 0 {
		return 0, fmt.Errorf("invalid bandwidth value: %q (use format like 1M, 500K, 1G)", s)
	}

	result := val * multiplier
	if val > 0 && result/multiplier != val {
		return 0, fmt.Errorf("bandwidth value overflow: %q", s)
	}

	return result, nil
}
