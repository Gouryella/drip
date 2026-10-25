package utils

import (
	"crypto/rand"
	"fmt"
	"regexp"
)

const (
	// SubdomainChars defines the allowed characters for subdomain generation
	SubdomainChars = "abcdefghijklmnopqrstuvwxyz0123456789"
	// DefaultSubdomainLength is the default length of generated subdomains
	DefaultSubdomainLength = 6
)

var subdomainRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// GenerateSubdomain generates a random subdomain
func GenerateSubdomain(length int) string {
	if length <= 0 {
		length = DefaultSubdomainLength
	}

	// Draw entropy in a single batch and map bytes onto the charset with
	// rejection sampling: mask each byte to 6 bits (0-63) and discard values
	// outside the charset. This keeps the distribution uniform while avoiding
	// one getrandom syscall and big.Int allocation per character.
	const mask = 0x3f
	var randomBytes [64]byte
	randomPos := len(randomBytes)
	charsLen := byte(len(SubdomainChars))

	result := make([]byte, length)
	for i := 0; i < length; {
		if randomPos == len(randomBytes) {
			if _, err := rand.Read(randomBytes[:]); err != nil {
				// crypto/rand should never fail in practice. If it does,
				// we cannot generate a secure random subdomain.
				panic(fmt.Sprintf("failed to generate random subdomain: %v", err))
			}
			randomPos = 0
		}

		b := randomBytes[randomPos] & mask
		randomPos++
		if b >= charsLen {
			continue
		}
		result[i] = SubdomainChars[b]
		i++
	}

	return string(result)
}

// ValidateSubdomain checks if a subdomain is valid
func ValidateSubdomain(subdomain string) bool {
	if len(subdomain) < 3 || len(subdomain) > 63 {
		return false
	}
	return subdomainRegex.MatchString(subdomain)
}

var reservedSubdomains = map[string]bool{
	"www":     true,
	"api":     true,
	"admin":   true,
	"app":     true,
	"mail":    true,
	"ftp":     true,
	"blog":    true,
	"shop":    true,
	"status":  true,
	"health":  true,
	"test":    true,
	"dev":     true,
	"staging": true,
}

// IsReserved checks if a subdomain is reserved
func IsReserved(subdomain string) bool {
	return reservedSubdomains[subdomain]
}
