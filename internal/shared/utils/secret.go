package utils

import (
	"crypto/subtle"
	"encoding/binary"
)

// ConstantTimeEqualString compares strings without leaking their contents or
// the position of the first differing byte through timing.
//
// Both inputs are prefixed with their length and padded to a common size, so
// the comparison is a single constant-time pass: a length mismatch does not
// cause an early return, and no hashing is required.
func ConstantTimeEqualString(a, b string) bool {
	n := max(len(a), len(b))

	aBuf := make([]byte, 8+n)
	bBuf := make([]byte, 8+n)
	binary.BigEndian.PutUint64(aBuf, uint64(len(a)))
	binary.BigEndian.PutUint64(bBuf, uint64(len(b)))
	copy(aBuf[8:], a)
	copy(bBuf[8:], b)

	return subtle.ConstantTimeCompare(aBuf, bBuf) == 1
}
