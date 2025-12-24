package utils

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// GenerateID generates a cryptographically secure random unique ID (32 hex chars).
// Panics if crypto/rand fails - this indicates a critical system issue.
func GenerateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// GenerateShortID generates a cryptographically secure shorter random ID (8 hex chars).
// Panics if crypto/rand fails.
func GenerateShortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// TryGenerateID returns an ID or error (for cases where panic is not desired).
func TryGenerateID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand failed: %w", err)
	}
	return hex.EncodeToString(b), nil
}
