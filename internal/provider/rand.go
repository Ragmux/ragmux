package provider

import (
	"crypto/rand"
	"encoding/base64"
)

// randomID returns 24 URL-safe random characters for synthesized IDs.
func randomID() string {
	const n = 24
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0000000000"
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}
