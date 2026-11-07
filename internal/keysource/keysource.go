// Package keysource opens the instance SECRET_KEY from Vault Transit or AWS KMS
// (ADR-038).
//
// The model is envelope encryption. The operator keeps a data key wrapped by a
// key that never leaves Vault or KMS; the wrapped blob is configuration
// (SECRET_KEY_WRAPPED). At startup the blob is opened once and the 32 plain
// bytes become the instance key exactly as if SECRET_KEY had held them, so the
// ciphertext format, the row-bound associated data and the canary are
// untouched and no request ever waits on Vault or KMS.
//
// Opening is fail-closed. A key that cannot be opened stops the process; there
// is no fallback to a key file, because a gateway that quietly minted its own
// key would seal new data nobody else can read. Nothing secret -- the token,
// the AWS credentials, the ciphertext, a response body -- is ever put in an
// error, a log line or a span: failures are reported as a class (auth, denied,
// network, malformed) plus an HTTP status.
package keysource

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Key sources, the values of SECRET_KEY_SOURCE.
const (
	// SourceEnv is today's behaviour: SECRET_KEY, SECRET_KEY_FILE or the
	// secret.key fallback. This package does nothing for it.
	SourceEnv = "env"
	// SourceVaultTransit opens the wrapped key with Vault's Transit engine.
	SourceVaultTransit = "vault_transit"
	// SourceAWSKMS opens the wrapped key with AWS KMS.
	SourceAWSKMS = "aws_kms"
)

const (
	// keyLen is the size of the instance key, AES-256.
	keyLen = 32
	// attempts is how many times opening is tried before the process stops.
	attempts = 3
	// attemptTimeout bounds one attempt, credential lookup included.
	attemptTimeout = 5 * time.Second
	// maxResponse caps what is read from any response.
	maxResponse = 1 << 20
)

// retryWaits are the pauses between attempts: one second, then three.
var retryWaits = []time.Duration{time.Second, 3 * time.Second}

// Options says where the instance key comes from. The zero value is SourceEnv.
type Options struct {
	// Source is one of the Source* constants; empty means SourceEnv.
	Source string
	// Wrapped is the sealed data key: a "vault:v1:..." string for Vault
	// Transit, the base64 CiphertextBlob for AWS KMS.
	Wrapped string

	// VaultAddr is the Vault base URL; https only.
	VaultAddr string
	// VaultTransitKey is the Transit key that wrapped the data key.
	VaultTransitKey string
	// VaultTransitMount is the Transit engine mount; empty means "transit".
	VaultTransitMount string
	// VaultToken authenticates the request. It is never logged.
	VaultToken string
	// VaultNamespace is sent as X-Vault-Namespace when set.
	VaultNamespace string
	// VaultCACert is a PEM file of extra roots trusted for VaultAddr.
	VaultCACert string

	// AWSRegion is the region of the KMS key.
	AWSRegion string
}

// Remote reports whether the key has to be fetched from Vault or KMS.
func (o Options) Remote() bool {
	return o.Source == SourceVaultTransit || o.Source == SourceAWSKMS
}

var regionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)

// Validate checks that the options are complete for their source. It looks at
// configuration only and makes no network call.
func (o Options) Validate() error {
	switch o.Source {
	case "", SourceEnv:
		if o.Wrapped != "" {
			return errors.New("SECRET_KEY_WRAPPED is set but SECRET_KEY_SOURCE is \"env\"; set SECRET_KEY_SOURCE=vault_transit or aws_kms, or remove it")
		}
		return nil
	case SourceVaultTransit:
		if o.Wrapped == "" {
			return errors.New("SECRET_KEY_SOURCE=vault_transit needs SECRET_KEY_WRAPPED (or SECRET_KEY_WRAPPED_FILE): the data key wrapped by the Transit key")
		}
		if o.VaultTransitKey == "" {
			return errors.New("SECRET_KEY_SOURCE=vault_transit needs VAULT_TRANSIT_KEY")
		}
		if o.VaultToken == "" {
			return errors.New("SECRET_KEY_SOURCE=vault_transit needs VAULT_TOKEN or VAULT_TOKEN_FILE")
		}
		u, err := url.Parse(o.VaultAddr)
		if o.VaultAddr == "" || err != nil || u.Host == "" {
			return errors.New("SECRET_KEY_SOURCE=vault_transit needs VAULT_ADDR, e.g. https://vault.example.com:8200")
		}
		if u.Scheme != "https" {
			return errors.New("VAULT_ADDR must be an https URL: the token and the key travel over it")
		}
		return nil
	case SourceAWSKMS:
		if o.Wrapped == "" {
			return errors.New("SECRET_KEY_SOURCE=aws_kms needs SECRET_KEY_WRAPPED (or SECRET_KEY_WRAPPED_FILE): the base64 CiphertextBlob of the data key")
		}
		if !regionPattern.MatchString(o.AWSRegion) {
			return errors.New("SECRET_KEY_SOURCE=aws_kms needs AWS_REGION (e.g. eu-west-1)")
		}
		return nil
	default:
		return fmt.Errorf("invalid SECRET_KEY_SOURCE %q (env, vault_transit or aws_kms)", o.Source)
	}
}

// Class names why opening failed. It is all an error or a log line says about
// the cause, on purpose.
type Class string

const (
	// ClassAuth means the caller could not prove who it is: a rejected or
	// expired token, no AWS credentials, a bad signature.
	ClassAuth Class = "auth"
	// ClassDenied means the identity is known and not allowed to decrypt.
	ClassDenied Class = "denied"
	// ClassNetwork means the service could not be reached or answered with a
	// server error or throttling.
	ClassNetwork Class = "network"
	// ClassMalformed means a request or response was not what the protocol
	// says: a bad ciphertext, an unparsable reply, a key that is not 32 bytes.
	// Asking again cannot change it, so it is the one class never retried.
	ClassMalformed Class = "malformed"
)

// Error is a failure to open the key. Its text is built from fixed strings, a
// class and an HTTP status; it never carries a token, a credential, the
// ciphertext or a response body.
type Error struct {
	Class Class
	// Op is the step that failed ("decrypt", "assume role", ...).
	Op string
	// Status is the HTTP status, or 0 when there was none.
	Status int
	// Detail is a fixed explanation or a transport error with the URL stripped.
	Detail string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s failed (class=%s", e.Op, e.Class)
	if e.Status != 0 {
		fmt.Fprintf(&b, ", http %d", e.Status)
	}
	b.WriteString(")")
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	return b.String()
}

// ClassOf returns the class of err, or "" when it is not an *Error.
func ClassOf(err error) Class {
	var ke *Error
	if errors.As(err, &ke) {
		return ke.Class
	}
	return ""
}

// Unwrap opens o.Wrapped with the configured service and returns the 32-byte
// key as 64 hex characters, the form every consumer of SECRET_KEY takes. It
// tries up to three times, five seconds each, pausing one and then three
// seconds. Network, auth and denied failures are all retried (a token that has
// not been projected yet, a role that is still propagating, an identity
// provider that answers late look exactly like a real refusal); a malformed
// request or reply stops at the first attempt (ADR-038).
func Unwrap(ctx context.Context, o Options, log *slog.Logger) (string, error) {
	return newResolver().unwrap(ctx, o, log)
}

// resolver carries the seams tests replace; Unwrap uses the real ones.
type resolver struct {
	getenv  func(string) string
	wait    func(ctx context.Context, d time.Duration) error
	timeout time.Duration
	waits   []time.Duration
	now     func() time.Time
	// client, when set, replaces the HTTP client of every call (tests trust
	// their httptest certificate through it).
	client httpDoer
}

func newResolver() *resolver {
	return &resolver{
		getenv:  os.Getenv,
		wait:    sleepCtx,
		timeout: attemptTimeout,
		waits:   retryWaits,
		now:     time.Now,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *resolver) unwrap(ctx context.Context, o Options, log *slog.Logger) (string, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := o.Validate(); err != nil {
		return "", err
	}
	var open func(context.Context) ([]byte, error)
	switch o.Source {
	case SourceVaultTransit:
		client, err := r.vaultClient(o)
		if err != nil {
			return "", fmt.Errorf("open SECRET_KEY via %s: %w", o.Source, err)
		}
		open = func(ctx context.Context) ([]byte, error) { return r.vaultDecrypt(ctx, client, o) }
	case SourceAWSKMS:
		open = func(ctx context.Context) ([]byte, error) { return r.kmsDecrypt(ctx, o) }
	default:
		return "", errors.New("keysource: SECRET_KEY_SOURCE=env has nothing to open")
	}

	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		actx, cancel := context.WithTimeout(ctx, r.timeout)
		key, err := open(actx)
		cancel()
		if err == nil {
			if len(key) != keyLen {
				n := len(key)
				wipe(key)
				err = &Error{Class: ClassMalformed, Op: "decrypt", Detail: fmt.Sprintf(
					"the opened key is %d bytes, want %d; wrap the 32 raw bytes, not their hex text (see docs/configuration.md#secret-key-sources)", n, keyLen)}
			} else {
				defer wipe(key)
				return hex.EncodeToString(key), nil
			}
		}
		last = err
		if ctx.Err() != nil {
			return "", fmt.Errorf("open SECRET_KEY via %s: %w", o.Source, ctx.Err())
		}
		log.Warn("secret key source: attempt failed", "source", o.Source, "attempt", attempt,
			"class", string(ClassOf(err)), "err", err.Error())
		if ClassOf(err) == ClassMalformed || attempt == attempts {
			break
		}
		if werr := r.wait(ctx, r.waits[min(attempt-1, len(r.waits)-1)]); werr != nil {
			return "", fmt.Errorf("open SECRET_KEY via %s: %w", o.Source, werr)
		}
	}
	return "", fmt.Errorf("open SECRET_KEY via %s: %w", o.Source, last)
}

// wipe overwrites b; the plain key should not outlive the hex string made from it.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
