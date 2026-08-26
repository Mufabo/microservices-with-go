package main

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns a random 128-bit identifier as a 32-character hex string.
// crypto/rand makes IDs unguessable, so an object key can't be enumerated.
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
