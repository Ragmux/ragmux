package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ragmux/ragmux/internal/config"
	"github.com/ragmux/ragmux/internal/store"
)

// rotateKey implements `ragmux rotate-key --new <hex>`: it opens the database
// with the current SECRET_KEY (or SECRET_KEY_FILE / the secret.key fallback),
// re-encrypts every provider credential with the new key in one transaction
// and prints how many rows changed. The gateway must then be restarted with
// the new key; until then it keeps working with the old one only for rows
// that were not rewritten, i.e. none, so stop it first.
func rotateKey(args []string) int {
	fs := flag.NewFlagSet("rotate-key", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	newKey := fs.String("new", "", "new SECRET_KEY as 64 hex characters (openssl rand -hex 32)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ragmux rotate-key --new <hex>\n\nRe-encrypts the stored provider API keys with a new SECRET_KEY. Stop the gateway first, run this with the current key in the environment, then start the gateway with the new key.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key := strings.TrimSpace(*newKey)
	if raw, err := hex.DecodeString(key); err != nil || len(raw) != 32 {
		fmt.Fprintln(os.Stderr, "rotate-key: --new must be 64 hex characters (32 bytes); generate one with: openssl rand -hex 32")
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	if key == cfg.SecretKeyHex {
		fmt.Fprintln(os.Stderr, "rotate-key: the new key equals the current SECRET_KEY")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, store.OpenConfig{DatabaseURL: cfg.DatabaseURL, MaxConns: 2,
		SecretKeyHex: cfg.SecretKeyHex, DataDir: cfg.DataDir}, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rotate-key:", err)
		return 1
	}
	defer func() { _ = st.Close() }()
	n, err := st.ReencryptConnections(ctx, key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rotate-key:", err)
		return 1
	}
	fmt.Printf("rotate-key: re-encrypted %d model connection(s); start the gateway with the new SECRET_KEY\n", n)
	return 0
}
