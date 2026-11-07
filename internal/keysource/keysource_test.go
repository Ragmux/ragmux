package keysource

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testKey = bytes.Repeat([]byte{0xab}, 32)

// Strings that must never reach an error message or a log line.
const (
	secretToken      = "hvs.SECRET-TOKEN-VALUE"
	secretCiphertext = "vault:v1:SECRETCIPHERTEXTBLOB"
	secretAccessKey  = "SECRETACCESSKEYVALUE0123456789abcdef"
	secretSession    = "SECRET-SESSION-TOKEN-VALUE"
	secretBody       = "LEAKY-RESPONSE-BODY-TEXT"
)

// testResolver has no real clock, no real waiting and a fixed environment.
func testResolver(env map[string]string, client httpDoer) (*resolver, *[]time.Duration) {
	var mu sync.Mutex
	waited := &[]time.Duration{}
	return &resolver{
		getenv: func(k string) string { return env[k] },
		wait: func(_ context.Context, d time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			*waited = append(*waited, d)
			return nil
		},
		timeout: 2 * time.Second,
		waits:   retryWaits,
		now:     func() time.Time { return suiteTime },
		client:  client,
	}, waited
}

// routeDoer serves requests from a handler in memory, recording each one.
type routeDoer struct {
	mu      sync.Mutex
	handler http.HandlerFunc
	reqs    []recorded
}

type recorded struct {
	Method, Host, Path string
	Header             http.Header
	Body               string
}

func (d *routeDoer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	d.mu.Lock()
	d.reqs = append(d.reqs, recorded{req.Method, req.URL.Host, req.URL.Path, req.Header.Clone(), string(body)})
	d.mu.Unlock()
	req.Host = req.URL.Host
	rec := httptest.NewRecorder()
	d.handler(rec, req)
	return rec.Result(), nil
}

func (d *routeDoer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.reqs)
}

func plainLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// noLeak fails when any known secret shows up in s.
func noLeak(t *testing.T, what, s string) {
	t.Helper()
	for _, secret := range []string{secretToken, secretCiphertext, secretAccessKey, secretSession, secretBody,
		hex.EncodeToString(testKey), base64.StdEncoding.EncodeToString(testKey)} {
		if strings.Contains(s, secret) {
			t.Errorf("%s leaks %q: %s", what, secret, s)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	good := Options{Source: SourceVaultTransit, Wrapped: "vault:v1:x", VaultAddr: "https://vault.test:8200",
		VaultTransitKey: "ragmux", VaultToken: "t"}
	for name, tc := range map[string]struct {
		mutate func(*Options)
		want   string // substring; empty means valid
	}{
		"vault ok":         {func(*Options) {}, ""},
		"vault no wrapped": {func(o *Options) { o.Wrapped = "" }, "SECRET_KEY_WRAPPED"},
		"vault no key":     {func(o *Options) { o.VaultTransitKey = "" }, "VAULT_TRANSIT_KEY"},
		"vault no token":   {func(o *Options) { o.VaultToken = "" }, "VAULT_TOKEN"},
		"vault no addr":    {func(o *Options) { o.VaultAddr = "" }, "VAULT_ADDR"},
		"vault http":       {func(o *Options) { o.VaultAddr = "http://vault.test" }, "https"},
		"unknown source":   {func(o *Options) { o.Source = "gcp" }, "invalid SECRET_KEY_SOURCE"},
		"kms ok":           {func(o *Options) { *o = Options{Source: SourceAWSKMS, Wrapped: "AAAA", AWSRegion: "eu-west-1"} }, ""},
		"kms no region":    {func(o *Options) { *o = Options{Source: SourceAWSKMS, Wrapped: "AAAA"} }, "AWS_REGION"},
		"kms bad region":   {func(o *Options) { *o = Options{Source: SourceAWSKMS, Wrapped: "AAAA", AWSRegion: "x.evil.com/#"} }, "AWS_REGION"},
		"kms no wrapped":   {func(o *Options) { *o = Options{Source: SourceAWSKMS, AWSRegion: "eu-west-1"} }, "SECRET_KEY_WRAPPED"},
		"env ok":           {func(o *Options) { *o = Options{} }, ""},
		"env with wrapped": {func(o *Options) { *o = Options{Source: SourceEnv, Wrapped: "x"} }, "SECRET_KEY_WRAPPED"},
	} {
		o := good
		tc.mutate(&o)
		err := o.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want it to mention %q", name, err, tc.want)
		}
	}
}

// vaultServer is a TLS Vault whose certificate is handed to the code through
// VAULT_CACERT, the way an operator would.
func vaultServer(t *testing.T, h http.HandlerFunc) (Options, *int) {
	t.Helper()
	calls := new(int)
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return Options{Source: SourceVaultTransit, Wrapped: secretCiphertext, VaultAddr: srv.URL,
		VaultTransitKey: "ragmux", VaultToken: secretToken, VaultCACert: ca}, calls
}

func vaultReply(plain []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": base64.StdEncoding.EncodeToString(plain)}})
	}
}

func TestVaultTransitOpensTheKey(t *testing.T) {
	var gotPath, gotToken, gotNS, gotBody string
	opts, calls := vaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotToken, gotNS = r.URL.Path, r.Header.Get("X-Vault-Token"), r.Header.Get("X-Vault-Namespace")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		vaultReply(testKey)(w, r)
	})
	opts.VaultNamespace = "team-a"
	r, waited := testResolver(nil, nil)

	got, err := r.unwrap(context.Background(), opts, plainLog(&bytes.Buffer{}))
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(testKey) {
		t.Errorf("key = %s", got)
	}
	if gotPath != "/v1/transit/decrypt/ragmux" {
		t.Errorf("path = %s (default mount is transit)", gotPath)
	}
	if gotToken != secretToken || gotNS != "team-a" {
		t.Errorf("headers: token %q namespace %q", gotToken, gotNS)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil || body["ciphertext"] != secretCiphertext {
		t.Errorf("body = %s", gotBody)
	}
	if *calls != 1 || len(*waited) != 0 {
		t.Errorf("calls %d, waits %v", *calls, *waited)
	}
}

func TestVaultCustomMount(t *testing.T) {
	var gotPath string
	opts, _ := vaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		vaultReply(testKey)(w, r)
	})
	opts.VaultTransitMount = "/team/transit/"
	opts.VaultTransitKey = "my key"
	r, _ := testResolver(nil, nil)
	if _, err := r.unwrap(context.Background(), opts, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/team/transit/decrypt/my%20key" {
		t.Errorf("path = %s", gotPath)
	}
}

// TLS verification is not optional: a server whose certificate the client was
// not told about is refused.
func TestVaultRefusesUntrustedCertificate(t *testing.T) {
	opts, _ := vaultServer(t, vaultReply(testKey))
	opts.VaultCACert = ""
	r, _ := testResolver(nil, nil)
	_, err := r.unwrap(context.Background(), opts, plainLog(&bytes.Buffer{}))
	if err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
	if !strings.Contains(err.Error(), "x509") {
		t.Errorf("error = %v", err)
	}
}

func TestVaultBadCACertFile(t *testing.T) {
	opts, _ := vaultServer(t, vaultReply(testKey))
	if err := os.WriteFile(opts.VaultCACert, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := testResolver(nil, nil)
	_, err := r.unwrap(context.Background(), opts, nil)
	if ClassOf(err) != ClassMalformed {
		t.Errorf("error = %v", err)
	}
}

func TestVaultClassesAndRetries(t *testing.T) {
	for _, tc := range []struct {
		status    int
		class     Class
		wantCalls int
	}{
		{http.StatusUnauthorized, ClassAuth, 3},
		{http.StatusForbidden, ClassDenied, 3},
		{http.StatusBadRequest, ClassMalformed, 1},
		{http.StatusNotFound, ClassMalformed, 1},
		{http.StatusServiceUnavailable, ClassNetwork, 3},
		{http.StatusTooManyRequests, ClassNetwork, 3},
		{http.StatusTemporaryRedirect, ClassNetwork, 3},
	} {
		opts, calls := vaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			if tc.status == http.StatusTemporaryRedirect {
				w.Header().Set("Location", "https://elsewhere.test/")
			}
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"errors":["`+secretBody+`"]}`)
		})
		r, waited := testResolver(nil, nil)
		var logs bytes.Buffer
		_, err := r.unwrap(context.Background(), opts, plainLog(&logs))
		if err == nil {
			t.Fatalf("%d: no error", tc.status)
		}
		if ClassOf(err) != tc.class {
			t.Errorf("%d: class %q, want %q (%v)", tc.status, ClassOf(err), tc.class, err)
		}
		if *calls != tc.wantCalls {
			t.Errorf("%d: %d call(s), want %d", tc.status, *calls, tc.wantCalls)
		}
		if tc.wantCalls == 3 && (len(*waited) != 2 || (*waited)[0] != time.Second || (*waited)[1] != 3*time.Second) {
			t.Errorf("%d: waits %v, want [1s 3s]", tc.status, *waited)
		}
		if !strings.Contains(logs.String(), "class="+string(tc.class)) {
			t.Errorf("%d: the class is not logged: %s", tc.status, logs.String())
		}
		noLeak(t, "error", err.Error())
		noLeak(t, "log", logs.String())
	}
}

func TestVaultSucceedsOnTheThirdAttempt(t *testing.T) {
	n := 0
	opts, calls := vaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		vaultReply(testKey)(w, r)
	})
	r, waited := testResolver(nil, nil)
	got, err := r.unwrap(context.Background(), opts, plainLog(&bytes.Buffer{}))
	if err != nil || got != hex.EncodeToString(testKey) {
		t.Fatalf("got %q, %v", got, err)
	}
	if *calls != 3 || len(*waited) != 2 {
		t.Errorf("calls %d, waits %v", *calls, *waited)
	}
}

func TestVaultServerDownIsNetwork(t *testing.T) {
	opts, _ := vaultServer(t, vaultReply(testKey))
	// A port nothing listens on.
	l := httptest.NewServer(http.NotFoundHandler())
	opts.VaultAddr = strings.Replace(l.URL, "http://", "https://", 1)
	l.Close()
	r, waited := testResolver(nil, nil)
	_, err := r.unwrap(context.Background(), opts, plainLog(&bytes.Buffer{}))
	if ClassOf(err) != ClassNetwork {
		t.Fatalf("error = %v", err)
	}
	if len(*waited) != 2 {
		t.Errorf("waits %v: a dead server must be retried twice", *waited)
	}
	noLeak(t, "error", err.Error())
	if strings.Contains(err.Error(), "/v1/transit") {
		t.Errorf("the request URL is in the error: %v", err)
	}
}

func TestKeyLengthIsChecked(t *testing.T) {
	for name, plain := range map[string][]byte{
		"short":      testKey[:16],
		"hex text":   []byte(hex.EncodeToString(testKey)),
		"one longer": append(append([]byte{}, testKey...), 1),
	} {
		opts, calls := vaultServer(t, vaultReply(plain))
		r, _ := testResolver(nil, nil)
		_, err := r.unwrap(context.Background(), opts, plainLog(&bytes.Buffer{}))
		if ClassOf(err) != ClassMalformed {
			t.Errorf("%s: error = %v", name, err)
			continue
		}
		if *calls != 1 {
			t.Errorf("%s: a malformed key was retried (%d calls)", name, *calls)
		}
		noLeak(t, name, err.Error())
	}
}

func TestVaultMalformedReply(t *testing.T) {
	for name, body := range map[string]string{
		"not json":  "<html>",
		"no data":   `{}`,
		"not b64":   `{"data":{"plaintext":"***"}}`,
		"empty ptx": `{"data":{"plaintext":""}}`,
	} {
		opts, _ := vaultServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
		r, _ := testResolver(nil, nil)
		_, err := r.unwrap(context.Background(), opts, nil)
		if ClassOf(err) != ClassMalformed {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestUnwrapStopsWhenContextEnds(t *testing.T) {
	opts, _ := vaultServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, waited := testResolver(nil, nil)
	_, err := r.unwrap(ctx, opts, nil)
	if err == nil || len(*waited) != 0 {
		t.Errorf("error %v, waits %v", err, *waited)
	}
}

func TestEnvSourceHasNothingToOpen(t *testing.T) {
	r, _ := testResolver(nil, nil)
	if _, err := r.unwrap(context.Background(), Options{}, nil); err == nil {
		t.Error("source env was opened")
	}
}

// kmsAPI answers KMS Decrypt calls and checks what a real KMS would: a signed
// request for the right target with the fixed encryption context.
func kmsAPI(t *testing.T, plain []byte, wantSession string, extra http.HandlerFunc) *routeDoer {
	t.Helper()
	d := &routeDoer{}
	d.handler = func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Host, "kms.") {
			if r.Header.Get("X-Amz-Target") != "TrentService.Decrypt" {
				t.Errorf("X-Amz-Target = %q", r.Header.Get("X-Amz-Target"))
			}
			if got := r.Header.Get("X-Amz-Security-Token"); got != wantSession {
				t.Errorf("X-Amz-Security-Token = %q, want %q", got, wantSession)
			}
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=") || !strings.Contains(auth, "/20150830/eu-west-1/kms/aws4_request") {
				t.Errorf("Authorization = %q", auth)
			}
			var req struct {
				CiphertextBlob    []byte
				EncryptionContext map[string]string
			}
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &req); err != nil {
				t.Errorf("body: %v", err)
			}
			if string(req.CiphertextBlob) != "wrapped-blob" {
				t.Errorf("CiphertextBlob = %q", req.CiphertextBlob)
			}
			if len(req.EncryptionContext) != 1 || req.EncryptionContext["ragmux:purpose"] != "secret_key" {
				t.Errorf("EncryptionContext = %v", req.EncryptionContext)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Plaintext": plain, "KeyId": "arn:aws:kms:eu-west-1:1:key/x"})
			return
		}
		extra(w, r)
	}
	return d
}

var kmsOptions = Options{Source: SourceAWSKMS, AWSRegion: "eu-west-1",
	Wrapped: base64.StdEncoding.EncodeToString([]byte("wrapped-blob"))}

func TestKMSWithStaticCredentials(t *testing.T) {
	d := kmsAPI(t, testKey, secretSession, func(http.ResponseWriter, *http.Request) {
		t.Error("the static credentials must be enough; nothing else may be called")
	})
	r, _ := testResolver(map[string]string{
		"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": secretAccessKey, "AWS_SESSION_TOKEN": secretSession,
		// Lower steps are configured too: the earlier one wins.
		"AWS_WEB_IDENTITY_TOKEN_FILE": "/nonexistent", "AWS_ROLE_ARN": "arn:aws:iam::1:role/x",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/creds",
	}, d)
	got, err := r.unwrap(context.Background(), kmsOptions, plainLog(&bytes.Buffer{}))
	if err != nil || got != hex.EncodeToString(testKey) {
		t.Fatalf("got %q, %v", got, err)
	}
	if d.count() != 1 {
		t.Errorf("%d requests", d.count())
	}
}

func TestKMSStaticCredentialsNeedTheSecret(t *testing.T) {
	r, _ := testResolver(map[string]string{"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE"}, &routeDoer{handler: func(http.ResponseWriter, *http.Request) {}})
	_, err := r.unwrap(context.Background(), kmsOptions, nil)
	if ClassOf(err) != ClassAuth {
		t.Errorf("error = %v", err)
	}
}

func TestKMSWebIdentityChain(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("  projected.jwt.token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := kmsAPI(t, testKey, "sts-session", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "sts.eu-west-1.amazonaws.com" || r.Method != http.MethodPost {
			t.Errorf("STS request %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("AssumeRoleWithWebIdentity must be unsigned")
		}
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		if form.Get("Action") != "AssumeRoleWithWebIdentity" || form.Get("RoleArn") != "arn:aws:iam::1:role/ragmux" ||
			form.Get("WebIdentityToken") != "projected.jwt.token" || form.Get("RoleSessionName") != "ragmux" {
			t.Errorf("form = %v", form)
		}
		_, _ = io.WriteString(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>
			<AccessKeyId>ASIAEXAMPLE</AccessKeyId><SecretAccessKey>`+secretAccessKey+`</SecretAccessKey>
			<SessionToken>sts-session</SessionToken></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`)
	})
	r, _ := testResolver(map[string]string{
		"AWS_WEB_IDENTITY_TOKEN_FILE": tokenFile, "AWS_ROLE_ARN": "arn:aws:iam::1:role/ragmux",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/creds",
	}, d)
	got, err := r.unwrap(context.Background(), kmsOptions, plainLog(&bytes.Buffer{}))
	if err != nil || got != hex.EncodeToString(testKey) {
		t.Fatalf("got %q, %v", got, err)
	}
	if d.count() != 2 {
		t.Errorf("%d requests, want STS then KMS", d.count())
	}
}

func TestKMSWebIdentityErrors(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("jwt-"+secretSession), 0o600)
	for _, tc := range []struct {
		status int
		code   string
		class  Class
	}{
		{400, "InvalidIdentityToken", ClassAuth},
		{403, "AccessDenied", ClassDenied},
		{503, "ServiceUnavailable", ClassNetwork},
		{400, "SomethingNew", ClassMalformed},
	} {
		d := &routeDoer{handler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `<ErrorResponse><Error><Code>`+tc.code+`</Code><Message>`+secretBody+`</Message></Error></ErrorResponse>`)
		}}
		r, _ := testResolver(map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": tokenFile, "AWS_ROLE_ARN": "arn:x"}, d)
		var logs bytes.Buffer
		_, err := r.unwrap(context.Background(), kmsOptions, plainLog(&logs))
		if ClassOf(err) != tc.class {
			t.Errorf("%s: error = %v", tc.code, err)
		}
		noLeak(t, tc.code, err.Error()+logs.String())
		if strings.Contains(err.Error()+logs.String(), "jwt-") {
			t.Errorf("%s: the web identity token leaked", tc.code)
		}
	}
	r, _ := testResolver(map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": filepath.Join(t.TempDir(), "missing"), "AWS_ROLE_ARN": "arn:x"}, &routeDoer{})
	if _, err := r.unwrap(context.Background(), kmsOptions, nil); ClassOf(err) != ClassAuth {
		t.Errorf("missing token file: %v", err)
	}
}

func TestKMSContainerCredentials(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "auth")
	_ = os.WriteFile(tokenFile, []byte("agent-auth-token\n"), 0o600)
	var seen recorded
	d := kmsAPI(t, testKey, "ctr-session", func(w http.ResponseWriter, r *http.Request) {
		seen = recorded{Method: r.Method, Host: r.URL.Host, Path: r.URL.Path, Header: r.Header.Clone()}
		_, _ = io.WriteString(w, `{"AccessKeyId":"ASIACTR","SecretAccessKey":"`+secretAccessKey+`","Token":"ctr-session"}`)
	})
	r, _ := testResolver(map[string]string{
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/abc", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": tokenFile,
	}, d)
	if got, err := r.unwrap(context.Background(), kmsOptions, nil); err != nil || got != hex.EncodeToString(testKey) {
		t.Fatalf("relative URI: %q, %v", got, err)
	}
	if seen.Host != "169.254.170.2" || seen.Path != "/v2/credentials/abc" || seen.Header.Get("Authorization") != "agent-auth-token" {
		t.Errorf("container request = %+v", seen)
	}

	// Pod Identity gives a full URI on its agent address.
	r, _ = testResolver(map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.23/v1/credentials"}, d)
	if _, err := r.unwrap(context.Background(), kmsOptions, nil); err != nil {
		t.Errorf("pod identity URI: %v", err)
	}
	// Anything else over plain HTTP is refused before a request is made.
	n := d.count()
	for _, bad := range []string{"http://example.com/creds", "ftp://169.254.170.2/x", "http://10.0.0.5/creds"} {
		r, _ = testResolver(map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": bad}, d)
		if _, err := r.unwrap(context.Background(), kmsOptions, nil); ClassOf(err) != ClassMalformed {
			t.Errorf("%s: error = %v", bad, err)
		}
	}
	if d.count() != n {
		t.Error("a refused URI was still requested")
	}
}

func TestKMSInstanceMetadataV2(t *testing.T) {
	d := kmsAPI(t, testKey, "imds-session", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				t.Error("no TTL on the IMDSv2 token request")
			}
			_, _ = io.WriteString(w, "imds-token")
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			if r.Header.Get("X-aws-ec2-metadata-token") != "imds-token" {
				t.Error("IMDSv2 token not presented")
			}
			_, _ = io.WriteString(w, "my-role\n")
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/my-role":
			_, _ = io.WriteString(w, `{"Code":"Success","AccessKeyId":"ASIAIMDS","SecretAccessKey":"`+secretAccessKey+`","Token":"imds-session"}`)
		default:
			t.Errorf("unexpected IMDS request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	r, _ := testResolver(nil, d)
	if got, err := r.unwrap(context.Background(), kmsOptions, nil); err != nil || got != hex.EncodeToString(testKey) {
		t.Fatalf("got %q, %v", got, err)
	}
	if d.count() != 4 {
		t.Errorf("%d requests, want token, role, credentials, KMS", d.count())
	}
}

func TestKMSNoCredentialsAnywhere(t *testing.T) {
	// IMDS refuses: this host has no role. An auth failure is tried three times
	// like any other (ADR-038), then the process stops.
	d := &routeDoer{handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }}
	r, waited := testResolver(nil, d)
	_, err := r.unwrap(context.Background(), kmsOptions, nil)
	if ClassOf(err) != ClassAuth || len(*waited) != 2 || d.count() != 3 {
		t.Errorf("error = %v, waits %v", err, *waited)
	}
	if err == nil || !strings.Contains(err.Error(), "no AWS credentials") {
		t.Errorf("the message does not say what is missing: %v", err)
	}

	// IMDS disabled: no request at all.
	d = &routeDoer{handler: func(http.ResponseWriter, *http.Request) {}}
	r, _ = testResolver(map[string]string{"AWS_EC2_METADATA_DISABLED": "true"}, d)
	if _, err := r.unwrap(context.Background(), kmsOptions, nil); ClassOf(err) != ClassAuth || d.count() != 0 {
		t.Errorf("disabled IMDS: %v, %d requests", err, d.count())
	}
}

func TestKMSErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		status    int
		typ       string
		class     Class
		wantCalls int
		shown     string
	}{
		{400, "AccessDeniedException", ClassDenied, 3, "AccessDeniedException"},
		{400, "com.amazon.coral.service#AccessDeniedException", ClassDenied, 3, "AccessDeniedException"},
		{400, "UnrecognizedClientException", ClassAuth, 3, "UnrecognizedClientException"},
		{400, "ExpiredTokenException", ClassAuth, 3, "ExpiredTokenException"},
		{400, "InvalidCiphertextException", ClassMalformed, 1, "InvalidCiphertextException"},
		{400, "IncorrectKeyException", ClassMalformed, 1, "IncorrectKeyException"},
		{400, "ThrottlingException", ClassNetwork, 3, "ThrottlingException"},
		{500, "KMSInternalException", ClassNetwork, 3, "KMSInternalException"},
		{503, "", ClassNetwork, 3, ""},
		{400, "ErrorFromTheFuture" + secretBody, ClassMalformed, 1, ""},
	} {
		d := &routeDoer{handler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"__type":"`+tc.typ+`","message":"`+secretBody+`"}`)
		}}
		r, _ := testResolver(map[string]string{"AWS_ACCESS_KEY_ID": "AKID", "AWS_SECRET_ACCESS_KEY": secretAccessKey}, d)
		var logs bytes.Buffer
		_, err := r.unwrap(context.Background(), kmsOptions, plainLog(&logs))
		if ClassOf(err) != tc.class {
			t.Errorf("%s: class %q, want %q (%v)", tc.typ, ClassOf(err), tc.class, err)
			continue
		}
		if d.count() != tc.wantCalls {
			t.Errorf("%s: %d call(s), want %d", tc.typ, d.count(), tc.wantCalls)
		}
		if tc.shown != "" && !strings.Contains(err.Error(), tc.shown) {
			t.Errorf("%s: the error name is not in %v", tc.typ, err)
		}
		noLeak(t, tc.typ, err.Error()+logs.String())
	}
}

func TestKMSBadConfiguration(t *testing.T) {
	d := &routeDoer{handler: func(http.ResponseWriter, *http.Request) { t.Error("request made") }}
	env := map[string]string{"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "b"}

	o := kmsOptions
	o.Wrapped = "***not base64***"
	r, _ := testResolver(env, d)
	if _, err := r.unwrap(context.Background(), o, nil); ClassOf(err) != ClassMalformed {
		t.Errorf("bad blob: %v", err)
	}

	for _, bad := range []string{"http://kms.internal", "kms.internal", "ftp://x"} {
		r, _ = testResolver(map[string]string{"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "b", "AWS_ENDPOINT_URL_KMS": bad}, d)
		if _, err := r.unwrap(context.Background(), kmsOptions, nil); ClassOf(err) != ClassMalformed {
			t.Errorf("endpoint %s: %v", bad, err)
		}
	}
}

func TestKMSEndpointOverrideAndChina(t *testing.T) {
	var hosts []string
	d := &routeDoer{handler: func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.URL.Host)
		_ = json.NewEncoder(w).Encode(map[string]any{"Plaintext": testKey})
	}}
	env := map[string]string{"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "b"}
	r, _ := testResolver(env, d)
	cn := kmsOptions
	cn.AWSRegion = "cn-north-1"
	_, _ = r.unwrap(context.Background(), cn, nil)

	env["AWS_ENDPOINT_URL_KMS"] = "https://vpce-1.kms.eu-west-1.vpce.amazonaws.com"
	_, _ = r.unwrap(context.Background(), kmsOptions, nil)
	want := []string{"kms.cn-north-1.amazonaws.com.cn", "vpce-1.kms.eu-west-1.vpce.amazonaws.com"}
	if strings.Join(hosts, ",") != strings.Join(want, ",") {
		t.Errorf("hosts = %v, want %v", hosts, want)
	}
}

// The real transport must never follow a redirect with the token on it.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("{}"))
	req.Header.Set("X-Vault-Token", secretToken)
	status, _, err := send(newClient(nil, false), req, "decrypt")
	if err != nil || status != http.StatusTemporaryRedirect || hit {
		t.Errorf("status %d, err %v, redirect followed: %v", status, err, hit)
	}
}

func TestUnwrapPublicEntryRejectsBadOptions(t *testing.T) {
	if _, err := Unwrap(context.Background(), Options{Source: SourceVaultTransit}, nil); err == nil {
		t.Error("incomplete options were accepted")
	}
}

// ADR-038: network, auth and denied are tried three times; malformed stops at
// the first answer. A refusal that clears up (a token not projected yet, a
// role still propagating) therefore opens the key on a later attempt.
func TestAuthAndDeniedAreRetriedMalformedIsNot(t *testing.T) {
	for _, tc := range []struct {
		status    int
		wantCalls int
	}{{http.StatusUnauthorized, 3}, {http.StatusForbidden, 3}, {http.StatusBadRequest, 1}} {
		n := 0
		opts, calls := vaultServer(t, func(w http.ResponseWriter, r *http.Request) {
			n++
			if n < 3 {
				w.WriteHeader(tc.status)
				return
			}
			vaultReply(testKey)(w, r)
		})
		r, waited := testResolver(nil, nil)
		got, err := r.unwrap(context.Background(), opts, plainLog(&bytes.Buffer{}))
		if tc.wantCalls == 3 {
			if err != nil || got != hex.EncodeToString(testKey) || *calls != 3 || len(*waited) != 2 {
				t.Errorf("%d: got %q, %v, %d calls, waits %v", tc.status, got, err, *calls, *waited)
			}
			continue
		}
		if err == nil || *calls != 1 || len(*waited) != 0 {
			t.Errorf("%d: error %v, %d calls, waits %v: malformed must stop at once", tc.status, err, *calls, *waited)
		}
	}
}

// The first attempt at STS fails with the transient InvalidIdentityToken and
// the second works: the key opens.
func TestWebIdentityTokenIsRetried(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte("jwt"), 0o600)
	sts := 0
	d := kmsAPI(t, testKey, "sts-session", func(w http.ResponseWriter, r *http.Request) {
		sts++
		if sts == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `<ErrorResponse><Error><Code>InvalidIdentityToken</Code></Error></ErrorResponse>`)
			return
		}
		_, _ = io.WriteString(w, `<R><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>A</AccessKeyId>
			<SecretAccessKey>S</SecretAccessKey><SessionToken>sts-session</SessionToken></Credentials></AssumeRoleWithWebIdentityResult></R>`)
	})
	r, waited := testResolver(map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": tokenFile, "AWS_ROLE_ARN": "arn:x"}, d)
	got, err := r.unwrap(context.Background(), kmsOptions, nil)
	if err != nil || got != hex.EncodeToString(testKey) || sts != 2 || len(*waited) != 1 {
		t.Errorf("got %q, %v, %d STS calls, waits %v", got, err, sts, *waited)
	}
}

// The source is named once, by the wrapper, not again inside the error.
func TestErrorNamesTheSourceOnce(t *testing.T) {
	opts, _ := vaultServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) })
	r, _ := testResolver(nil, nil)
	_, err := r.unwrap(context.Background(), opts, nil)
	if err == nil || strings.Count(err.Error(), "vault_transit") != 1 {
		t.Errorf("error = %v", err)
	}
}
