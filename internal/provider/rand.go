package provider

import (
	"crypto/rand"
	"encoding/base64"
)

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0000000000"
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}
