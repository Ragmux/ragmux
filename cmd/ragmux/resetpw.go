package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/config"
	"github.com/ragmux/ragmux/internal/store"
)

// resetPassword implements `ragmux reset-password <username>`: the way back
// in when every administrator is locked out of the dashboard. It needs the
// database and SECRET_KEY, not a running gateway, and grants no new
// privilege: whoever holds DATABASE_URL and SECRET_KEY can already rewrite
// any row by hand.
//
// The password is taken from --generate or --stdin only. There is
// deliberately no --password flag and no interactive prompt: a flag value
// lands in `ps` output and the shell history, and a no-echo prompt needs
// golang.org/x/term, which is not in go.mod and is not worth a dependency
// for one emergency command.

// resetMinPasswordLen matches the first-run setup rather than the
// dashboard's 8: an emergency admin reset should not mint a weak credential.
const resetMinPasswordLen = 12

// generatedPasswordLen is the length of a --generate password, in
// store.GenerateSessionToken's alphabet.
const generatedPasswordLen = 24

func resetPassword(args []string) int {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	generate := fs.Bool("generate", false, "generate a 24-character password and print it")
	fromStdin := fs.Bool("stdin", false, "read the new password from the first line of stdin")
	force := fs.Bool("force", false, "do not ask for confirmation")
	noRevoke := fs.Bool("no-revoke", false, "keep the user's dashboard sessions alive")
	revokeKeys := fs.Bool("revoke-keys", false, "also revoke every api key the user owns")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ragmux reset-password <username> [--generate|--stdin] [--force] [--no-revoke] [--revoke-keys]\n\nSets a new password for a dashboard account directly in the database, for when every administrator is locked out. Needs DATABASE_URL (and SECRET_KEY); the gateway does not have to be running.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Go's flag package stops at the first non-flag argument, so the username
	// is taken out and what follows it is parsed again; flags may then be
	// written on either side of it.
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	username := strings.TrimSpace(rest[0])
	if err := fs.Parse(rest[1:]); err != nil {
		return 2
	}
	if username == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if *generate == *fromStdin {
		fmt.Fprintln(os.Stderr, "reset-password: pass exactly one of --generate and --stdin; there is no --password flag because a flag value leaks into ps and the shell history")
		return 2
	}

	password := ""
	if *generate {
		p, err := store.GeneratePassword(generatedPasswordLen)
		if err != nil {
			fmt.Fprintln(os.Stderr, "reset-password:", err)
			return 1
		}
		password = p
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintln(os.Stderr, "reset-password: read stdin:", err)
			return 1
		}
		password = strings.TrimRight(line, "\r\n")
	}
	if msg := checkResetPassword(password); msg != "" {
		fmt.Fprintln(os.Stderr, "reset-password:", msg)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(ctx, store.OpenConfig{DatabaseURL: cfg.DatabaseURL, MaxConns: 2,
		SecretKeyHex: cfg.SecretKeyHex, DataDir: cfg.DataDir}, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reset-password:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	// The account has to exist: this command never creates one, so a typo
	// cannot quietly add an administrator.
	u, err := st.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "reset-password: no user named %q; this command never creates accounts\n", username)
			return 1
		}
		fmt.Fprintln(os.Stderr, "reset-password:", err)
		return 1
	}
	// --stdin already consumed stdin, so there is nothing left to read a
	// confirmation from; --force skips it explicitly.
	if !*force && !*fromStdin && isTerminal(os.Stdin) {
		fmt.Printf("Reset the password of %q (%s, active=%v)? [y/N] ", u.Username, u.Role, u.IsActive)
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(os.Stderr, "reset-password: cancelled")
			return 1
		}
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reset-password:", err)
		return 1
	}
	if err := st.UpdateUserPassword(ctx, u.ID, hash); err != nil {
		fmt.Fprintln(os.Stderr, "reset-password:", err)
		return 1
	}
	var sessions int64
	if !*noRevoke {
		// A forgotten password is how a suspected compromise surfaces, so
		// the old sessions go unless the operator opts out.
		if sessions, err = st.RevokeUserSessions(ctx, u.ID); err != nil {
			fmt.Fprintln(os.Stderr, "reset-password:", err)
			return 1
		}
	}
	var keys int64
	if *revokeKeys {
		if keys, err = st.RevokeUserAPIKeys(ctx, u.ID); err != nil {
			fmt.Fprintln(os.Stderr, "reset-password:", err)
			return 1
		}
	}

	// audit_logs.actor_user_id is already nullable, so the CLI needs no
	// migration to record itself: it is an actor without an account.
	details := map[string]any{"via": "cli", "host": hostname(), "os_user": osUser(),
		"revoked_sessions": sessions, "revoked_keys": keys}
	if err := st.InsertAuditLog(ctx, &store.AuditLog{ActorUsername: "cli", Action: "user.reset_password",
		TargetType: "user", TargetID: &u.ID, Details: details, IP: "cli"}); err != nil {
		fmt.Fprintln(os.Stderr, "reset-password: warning: could not write the audit entry:", err)
	}

	fmt.Printf("reset-password: password of %q updated", u.Username)
	if !*noRevoke {
		fmt.Printf("; %d dashboard session(s) revoked", sessions)
	}
	if *revokeKeys {
		fmt.Printf("; %d api key(s) revoked", keys)
	}
	fmt.Println()
	if *generate {
		fmt.Printf("new password: %s\n", password)
	}
	if !u.IsActive {
		fmt.Printf("note: %q is deactivated and still cannot sign in; reactivate it in the dashboard\n", u.Username)
	}
	return 0
}

// checkResetPassword is the validation message for the new password, or "".
func checkResetPassword(pw string) string {
	switch {
	case len(pw) < resetMinPasswordLen:
		return fmt.Sprintf("the new password must be at least %d characters", resetMinPasswordLen)
	case len(pw) > 72:
		return "the new password must be at most 72 bytes (bcrypt's input limit)"
	}
	return ""
}

// isTerminal reports whether f is a character device, which is as much as
// can be told without golang.org/x/term.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

func osUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}
