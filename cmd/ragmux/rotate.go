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
	"github.com/ragmux/ragmux/internal/keysource"
	"github.com/ragmux/ragmux/internal/store"
)

// rotateKey implements `ragmux rotate-key --new <hex>` and `--new-wrapped
// <blob>`: it opens the database with the current key, from wherever
// SECRET_KEY_SOURCE says it lives (SECRET_KEY / SECRET_KEY_FILE / the
// secret.key fallback, or Vault Transit / AWS KMS), re-encrypts every provider
// credential and every other secret the key protects (webhook secrets, TOTP
// secrets, the canary) with the new key in one transaction and prints how many
// rows changed. The gateway must then be restarted with the new key; until
// then it keeps working with the old one only for rows that were not
// rewritten, i.e. none, so stop it first.
//
// --new-wrapped is the Vault / KMS form: the blob is opened with the same
// source and the same credentials as the current key. Under those sources
// --new is refused (ADR-038), so no rotation can commit to a key that exists
// only on the command line.
func rotateKey(args []string) int {
	fs := flag.NewFlagSet("rotate-key", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	newKey := fs.String("new", "", "new SECRET_KEY as 64 hex characters (openssl rand -hex 32)")
	newWrapped := fs.String("new-wrapped", "", "new data key wrapped by Vault Transit or AWS KMS (needs SECRET_KEY_SOURCE=vault_transit or aws_kms); it is opened with the same source as the current key")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ragmux rotate-key --new <hex> | --new-wrapped <blob>\n\nWith SECRET_KEY_SOURCE=vault_transit or aws_kms only --new-wrapped is accepted: wrap the new key first, then rotate.\n\nRe-encrypts the stored provider API keys, webhook secrets and TOTP secrets with a new SECRET_KEY. Stop the gateway first, run this with the current key available the way the gateway gets it (SECRET_KEY_SOURCE and its settings), then start the gateway with the new key.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key := strings.TrimSpace(*newKey)
	wrapped := strings.TrimSpace(*newWrapped)
	if (key == "") == (wrapped == "") {
		fmt.Fprintln(os.Stderr, "rotate-key: pass exactly one of --new <hex> and --new-wrapped <blob>")
		return 2
	}
	if key != "" {
		if raw, err := hex.DecodeString(key); err != nil || len(raw) != 32 {
			fmt.Fprintln(os.Stderr, "rotate-key: --new must be 64 hex characters (32 bytes); generate one with: openssl rand -hex 32")
			return 2
		}
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	if cfg.KeySource.Remote() && key != "" {
		// ADR-038: a key given as hex is stored nowhere, and a gateway on a
		// remote source can only start from a wrapped blob of exactly that key.
		// --new-wrapped asks the same Vault / KMS to open a key that was
		// wrapped BEFORE the rotation, so it cannot strand the database. This
		// check runs before the current key is opened and before the database
		// is touched.
		fmt.Fprintf(os.Stderr, "rotate-key: --new is not accepted with SECRET_KEY_SOURCE=%s: a key given as hex is stored nowhere, "+
			"and after the rotation the gateway can only start from a wrapped key. Wrap the new 32-byte key with %s first "+
			"and pass the result as --new-wrapped <blob> (see docs/configuration.md#moving-to-a-key-source-and-rotating)\n",
			cfg.KeySource.Source, cfg.KeySource.Source)
		return 2
	}
	if wrapped != "" && !cfg.KeySource.Remote() {
		fmt.Fprintln(os.Stderr, "rotate-key: --new-wrapped needs SECRET_KEY_SOURCE=vault_transit or aws_kms; with the env source use --new <hex>")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	current, source, err := resolveSecretKey(ctx, cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rotate-key: secret key:", err)
		return 1
	}
	if wrapped != "" {
		opts := cfg.KeySource
		opts.Wrapped = wrapped
		if key, err = keysource.Unwrap(ctx, opts, log); err != nil {
			fmt.Fprintln(os.Stderr, "rotate-key: --new-wrapped:", err)
			return 1
		}
	}
	if current != "" && key == current {
		fmt.Fprintln(os.Stderr, "rotate-key: the new key equals the current SECRET_KEY")
		return 2
	}
	st, err := store.Open(ctx, store.OpenConfig{DatabaseURL: cfg.DatabaseURL, MaxConns: 2,
		SecretKeyHex: current, SecretKeySource: source, DataDir: cfg.DataDir}, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rotate-key:", err)
		return 1
	}
	defer func() { _ = st.Close() }()
	rep, err := st.ReencryptSealed(ctx, key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rotate-key:", err)
		return 1
	}
	fmt.Print(rotateSummary(rep))
	if wrapped != "" {
		fmt.Println("rotate-key: set SECRET_KEY_WRAPPED (or SECRET_KEY_WRAPPED_FILE) to the --new-wrapped value before starting the gateway")
	}
	return 0
}

// rotateSummary is the success output of rotate-key: the connection line the
// command has always printed, then one line per registered sealed column that
// held rows.
func rotateSummary(rep store.SealedReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rotate-key: re-encrypted %d model connection(s)", rep.Connections)
	b.WriteString("; start the gateway with the new SECRET_KEY\n")
	for _, c := range rep.Columns {
		if c.Rows > 0 {
			fmt.Fprintf(&b, "rotate-key: re-encrypted %d %s.%s value(s)\n", c.Rows, c.Table, c.Column)
		}
	}
	return b.String()
}
