package store

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

var plainIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// The registry's names are interpolated into SQL; they must stay plain
// identifiers, and every column must bind its blob to the row with a prefix
// of its own.
func TestSealedRegistryEntriesAreSafeToInterpolate(t *testing.T) {
	if len(sealedColumns) == 0 {
		t.Fatal("the registry is empty")
	}
	seen := map[string]bool{}
	for _, c := range sealedColumns {
		for name, v := range map[string]string{"table": c.table, "idCol": c.idCol, "blobCol": c.blobCol} {
			if !plainIdentifier.MatchString(v) {
				t.Errorf("%s.%s: %s %q is not a plain identifier", c.table, c.blobCol, name, v)
			}
		}
		if c.versionCol != "" && !plainIdentifier.MatchString(c.versionCol) {
			t.Errorf("%s.%s: versionCol %q is not a plain identifier", c.table, c.blobCol, c.versionCol)
		}
		if !strings.HasPrefix(c.aadPrefix, "ragmux:") || !strings.HasSuffix(c.aadPrefix, ":") {
			t.Errorf("%s.%s: aadPrefix %q should look like ragmux:<kind>:", c.table, c.blobCol, c.aadPrefix)
		}
		if c.aadPrefix == "ragmux:model_connection:" {
			t.Errorf("%s.%s: aadPrefix %q collides with the connection prefix", c.table, c.blobCol, c.aadPrefix)
		}
		key := c.table + "." + c.blobCol
		if seen[key] {
			t.Errorf("%s registered twice", key)
		}
		seen[key] = true
	}
	for _, want := range []string{"webhook_endpoints.secret_enc", "webhook_endpoints.prev_secret_enc"} {
		if !seen[want] {
			t.Errorf("%s is not in the registry; rotate-key would strand it", want)
		}
	}
}

func TestSealedAADIsPrefixPlusID(t *testing.T) {
	if got := string(sealedAAD(webhookAADPrefix, 42)); got != "ragmux:webhook_endpoint:42" {
		t.Errorf("aad = %q", got)
	}
	if string(sealedAAD(webhookAADPrefix, 4)) == string(sealedAAD(webhookAADPrefix, 42)) {
		t.Error("distinct ids share an aad")
	}
	if string(sealedAAD(webhookAADPrefix, 1)) == string(connectionAAD(1)) {
		t.Error("a webhook secret and a connection key share an aad")
	}
}

// A previous secret that does not open must not take the endpoint down; the
// current one must open, or the endpoint is reported unreadable.
func TestOpenEndpointSecretsToleratesAnUnreadablePreviousSecret(t *testing.T) {
	c, err := newCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{cipher: c, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	aad := sealedAAD(webhookAADPrefix, 7)
	cur, err := c.encrypt("whsec_current", aad)
	if err != nil {
		t.Fatal(err)
	}

	e := &WebhookEndpoint{ID: 7}
	if err := s.openEndpointSecrets(e, cur, bytes.Repeat([]byte{9}, 64)); err != nil {
		t.Fatalf("garbage previous secret failed the endpoint: %v", err)
	}
	if e.Secret != "whsec_current" || e.PrevSecret != "" {
		t.Errorf("secret/prev = %q/%q", e.Secret, e.PrevSecret)
	}

	// A current secret sealed for another row does not open here.
	other, _ := c.encrypt("whsec_current", sealedAAD(webhookAADPrefix, 8))
	e = &WebhookEndpoint{ID: 7}
	if err := s.openEndpointSecrets(e, other, nil); !errors.Is(err, errSecretUnreadable) {
		t.Errorf("foreign secret: %v, want errSecretUnreadable", err)
	}
	if err := s.openEndpointSecrets(&WebhookEndpoint{ID: 7}, bytes.Repeat([]byte{1}, 64), nil); !errors.Is(err, errSecretUnreadable) {
		t.Errorf("garbage secret: %v, want errSecretUnreadable", err)
	}
}
