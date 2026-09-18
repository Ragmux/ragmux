package store

import (
	"crypto/aes"
	cryptocipher "crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cipher encrypts provider credentials at rest with AES-256-GCM.
type cipher struct {
	aead cryptocipher.AEAD
}

// loadCipher builds the credential cipher from the hex key when given, and
// otherwise from (or into) dataDir/secret.key. It returns the source used.
func loadCipher(keyHex, dataDir string, log *slog.Logger) (*cipher, string, error) {
	if keyHex != "" {
		key, err := hex.DecodeString(strings.TrimSpace(keyHex))
		if err != nil || len(key) != 32 {
			return nil, "", errors.New("SECRET_KEY must be 64 hex characters (32 bytes)")
		}
		c, err := newCipher(key)
		return c, "env", err
	}
	if dataDir == "" {
		return nil, "", errors.New("SECRET_KEY is unset and no data directory is configured for the secret.key fallback")
	}
	log.Warn("SECRET_KEY is not set; falling back to a key file. Set SECRET_KEY (openssl rand -hex 32) so credentials survive container recreation.",
		"path", filepath.Join(dataDir, "secret.key"))
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, "", fmt.Errorf("create data dir for secret.key fallback (set SECRET_KEY to avoid this): %w", err)
	}
	c, err := loadOrCreateCipher(filepath.Join(dataDir, "secret.key"))
	return c, "file", err
}

func loadOrCreateCipher(path string) (*cipher, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
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

// Key versions recorded in model_connections.key_version.
const (
	// keyVersionLegacy sealed the credential without associated data.
	keyVersionLegacy int16 = 0
	// keyVersionBound binds the ciphertext to the connection id, so a blob
	// copied onto another row (by someone with database write access) no
	// longer decrypts there.
	keyVersionBound int16 = 1
)

// connectionAAD is the associated data for a connection credential at
// keyVersionBound.
func connectionAAD(id int64) []byte {
	return []byte("ragmux:model_connection:" + strconv.FormatInt(id, 10))
}

// aadFor returns the associated data to use for a stored key version.
func aadFor(version int16, id int64) ([]byte, error) {
	switch version {
	case keyVersionLegacy:
		return nil, nil
	case keyVersionBound:
		return connectionAAD(id), nil
	default:
		return nil, fmt.Errorf("unknown key version %d", version)
	}
}

// encrypt seals plain with a fresh nonce; aad is authenticated but not
// stored and must be supplied again to decrypt. An empty plaintext yields a
// nil blob.
func (c *cipher) encrypt(plain string, aad []byte) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, c.aead.Seal(nil, nonce, []byte(plain), aad)...), nil
}

// decrypt opens a blob produced by encrypt with the same aad.
func (c *cipher) decrypt(blob, aad []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	n := c.aead.NonceSize()
	if len(blob) < n {
		return "", errors.New("ciphertext too short")
	}
	out, err := c.aead.Open(nil, blob[:n], blob[n:], aad)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
