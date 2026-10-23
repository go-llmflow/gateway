package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// KeyPrefix starts every virtual key, so a leaked key is recognisable by
// secret scanners.
const KeyPrefix = "llmf-"

// GenerateKey returns a new secret (256 random bits), its public ID and
// its SHA-256. The secret is shown once to the caller and never stored.
// An unsalted hash is enough for a uniformly random 256-bit secret: there
// is no dictionary to precompute.
func GenerateKey() (secret, id string, hash [32]byte) {
	var b [32]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go >= 1.24)
	secret = KeyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	var idb [8]byte
	_, _ = rand.Read(idb[:])
	id = "k_" + hex.EncodeToString(idb[:])
	return secret, id, HashSecret(secret)
}

// HashSecret hashes a presented key.
func HashSecret(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

// HashEqual compares two hashes in constant time.
func HashEqual(a, b [32]byte) bool { return subtle.ConstantTimeCompare(a[:], b[:]) == 1 }
