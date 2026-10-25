package utils

import (
	"crypto/sha256"
	"crypto/subtle"
)

// ConstantTimeEqualString compares strings without leaking their original
// lengths through subtle.ConstantTimeCompare's early length check.
func ConstantTimeEqualString(a, b string) bool {
	aDigest := sha256.Sum256([]byte(a))
	bDigest := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aDigest[:], bDigest[:]) == 1
}
