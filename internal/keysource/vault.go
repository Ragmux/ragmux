package keysource

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// httpDoer is the part of *http.Client the key sources use.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// noRedirect makes a redirect the answer instead of something to follow: the
// Vault token and the signed AWS request must not be replayed at a host the
// operator did not name.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// newClient returns an HTTP client that verifies TLS against pool (the system
// roots when nil) and never follows a redirect. useProxy lets HTTP_PROXY and
// friends apply, which Vault and the AWS APIs honour but instance metadata
// must not.
func newClient(pool *x509.CertPool, useProxy bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	tr.DisableKeepAlives = true
	if !useProxy {
		tr.Proxy = nil
	}
	return &http.Client{Transport: tr, CheckRedirect: noRedirect}
}

// send runs req and reads at most maxResponse bytes of the reply. A transport
// failure is a network error whose text has the URL stripped.
func send(client httpDoer, req *http.Request, op string) (int, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, &Error{Class: ClassNetwork, Op: op, Detail: transportDetail(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return resp.StatusCode, nil, &Error{Class: ClassNetwork, Op: op, Status: resp.StatusCode,
			Detail: "reading the response: " + transportDetail(err)}
	}
	return resp.StatusCode, body, nil
}

// transportDetail describes a transport error without the request URL, which
// net/http puts in every *url.Error.
func transportDetail(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// classByStatus is the class of a non-success HTTP status when the service
// said nothing more precise.
func classByStatus(status int) Class {
	switch {
	case status == http.StatusUnauthorized:
		return ClassAuth
	case status == http.StatusForbidden:
		return ClassDenied
	case status == http.StatusTooManyRequests, status >= 500, status >= 300 && status < 400:
		return ClassNetwork
	default:
		return ClassMalformed
	}
}

// vaultClient builds the HTTP client for Vault: system roots plus
// VAULT_CACERT. Verification is never switched off.
func (r *resolver) vaultClient(o Options) (httpDoer, error) {
	if r.client != nil {
		return r.client, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if o.VaultCACert != "" {
		pem, err := os.ReadFile(filepath.Clean(o.VaultCACert))
		if err != nil {
			return nil, &Error{Class: ClassMalformed, Op: "load VAULT_CACERT",
				Detail: transportDetail(err)}
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, &Error{Class: ClassMalformed, Op: "load VAULT_CACERT",
				Detail: "the file holds no PEM certificate"}
		}
	}
	return newClient(pool, true), nil
}

// vaultDecrypt opens the wrapped key with POST /v1/<mount>/decrypt/<key> and
// returns the plaintext bytes (Vault returns them base64-encoded).
func (r *resolver) vaultDecrypt(ctx context.Context, client httpDoer, o Options) ([]byte, error) {
	const op = "decrypt"
	mount := strings.Trim(o.VaultTransitMount, "/")
	if mount == "" {
		mount = "transit"
	}
	segs := strings.Split(mount, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	endpoint := strings.TrimRight(o.VaultAddr, "/") + "/v1/" + strings.Join(segs, "/") +
		"/decrypt/" + url.PathEscape(o.VaultTransitKey)
	payload, err := json.Marshal(map[string]string{"ciphertext": o.Wrapped})
	if err != nil {
		return nil, &Error{Class: ClassMalformed, Op: op, Detail: "encode the request"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, &Error{Class: ClassMalformed, Op: op, Detail: "VAULT_ADDR, VAULT_TRANSIT_MOUNT or VAULT_TRANSIT_KEY is not a usable URL part"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", o.VaultToken)
	if o.VaultNamespace != "" {
		req.Header.Set("X-Vault-Namespace", o.VaultNamespace)
	}
	status, body, err := send(client, req, op)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		detail := ""
		switch status {
		case http.StatusUnauthorized:
			detail = "Vault rejected the token"
		case http.StatusForbidden:
			detail = "Vault refused: the token is invalid or its policy does not allow transit decrypt on this key"
		case http.StatusBadRequest:
			detail = "Vault could not decrypt the wrapped key (wrong key name or a ciphertext this key did not produce)"
		case http.StatusNotFound:
			detail = "Vault has no such transit mount or key"
		}
		return nil, &Error{Class: classByStatus(status), Op: op, Status: status, Detail: detail}
	}
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Data.Plaintext == "" {
		return nil, &Error{Class: ClassMalformed, Op: op, Status: status,
			Detail: "the reply has no data.plaintext"}
	}
	plain, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil {
		return nil, &Error{Class: ClassMalformed, Op: op, Status: status,
			Detail: "data.plaintext is not base64"}
	}
	return plain, nil
}
