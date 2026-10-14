package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

const RedactedLogValue = "[REDACTED]"

// IsSensitiveLogKey reports whether a query/form/header key commonly carries
// credentials or bearer material that should not be written to logs.
func IsSensitiveLogKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	if normalized == "" {
		return false
	}

	switch normalized {
	case "access_token",
		"api_key",
		"apikey",
		"auth",
		"authorization",
		"bearer",
		"client_secret",
		"code",
		"id_token",
		"jwt",
		"key",
		"otp",
		"pass",
		"passwd",
		"password",
		"refresh_token",
		"secret",
		"session",
		"sid",
		"sig",
		"signature",
		"signed",
		"token":
		return true
	}

	return strings.Contains(normalized, "token") ||
		strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "credential")
}

// RedactQueryValuesForLog returns a copy of values with sensitive keys redacted.
func RedactQueryValuesForLog(values url.Values) url.Values {
	redacted := make(url.Values, len(values))
	for key, vals := range values {
		copied := make([]string, len(vals))
		if IsSensitiveLogKey(key) {
			for i := range copied {
				copied[i] = RedactedLogValue
			}
		} else {
			copy(copied, vals)
		}
		redacted[key] = copied
	}
	return redacted
}

// RedactedRawQueryForLog returns an encoded query string with sensitive values
// replaced. Prefer QueryKeysForLog for request logs that do not need values.
func RedactedRawQueryForLog(u *url.URL) string {
	if u == nil || u.RawQuery == "" {
		return ""
	}
	return RedactQueryValuesForLog(u.Query()).Encode()
}

// QueryKeysForLog returns a stable, lowercase list of query keys without values.
func QueryKeysForLog(u *url.URL) []string {
	if u == nil || u.RawQuery == "" {
		return nil
	}

	seen := make(map[string]struct{})
	for key := range u.Query() {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if normalized == "" {
			continue
		}
		seen[normalized] = struct{}{}
	}

	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// URLPathForLog returns only the request path, never the raw query string.
func URLPathForLog(u *url.URL) string {
	if u == nil {
		return ""
	}
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// HashForLog returns a short, deterministic digest for correlating log lines
// without writing raw identifiers such as IPs, subdomains, or tokens.
func HashForLog(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}
