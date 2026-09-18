package store

import (
	"crypto/aes"
	cryptocipher "crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// cipher encrypts provider credentials at rest with AES-256-GCM. The key is
// generated on first start and kept in the data directory so an existing
// database stays readable after the container is recreated.
type cipher struct {
	aead cryptocipher.AEAD
}

func loadOrCreateCipher(path string) (*cipher, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, derr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if derr != nil || len(key) != 32 {
			return nil, fmt.Errorf("secret key at %s is malformed", path)
		}
		return newCipher(key)
	case errors.Is(err, os.ErrNotExist):
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write secret key: %w", err)
		}
		return newCipher(key)
	default:
		return nil, fmt.Errorf("read secret key: %w", err)
	}
}

func newCipher(key []byte) (*cipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cryptocipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &cipher{aead: aead}, nil
}

func (c *cipher) encrypt(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, c.aead.Seal(nil, nonce, []byte(plain), nil)...), nil
}

func (c *cipher) decrypt(blob []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	n := c.aead.NonceSize()
	if len(blob) < n {
		return "", errors.New("ciphertext too short")
	}
	out, err := c.aead.Open(nil, blob[:n], blob[n:], nil)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
