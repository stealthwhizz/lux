// Package ids generates prefixed random identifiers and secrets.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

var enc = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return enc.EncodeToString(b)
}

// New returns an identifier like "task_5f2k9...". 10 random bytes = 80 bits.
func New(prefix string) string { return prefix + "_" + random(10) }

// Secret returns a high-entropy secret (256 bits) with a recognizable prefix.
func Secret(prefix string) string { return prefix + "_" + random(32) }

// Hash returns the SHA-256 of a secret, as stored in the database.
func Hash(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// HasPrefix reports whether s looks like a secret with the given prefix.
func HasPrefix(s, prefix string) bool { return strings.HasPrefix(s, prefix+"_") }

// HashHex is Hash as hexadecimal text, for storing in JSON.
func HashHex(s string) string { return hex.EncodeToString(Hash(s)) }
