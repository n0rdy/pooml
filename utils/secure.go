package utils

import (
	"crypto/sha256"
	"crypto/subtle"
)

// SecureCompare is constant-time in content AND length: a raw
// subtle.ConstantTimeCompare returns immediately on length mismatch,
// leaking the secret's length. Hashing first makes both sides fixed-size.
func SecureCompare(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}
