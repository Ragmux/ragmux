package keysource

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The vectors below are from AWS's published Signature Version 4 test suite
// ("aws-sig-v4-test-suite", service "service", region us-east-1) and from the
// signing key example in the SigV4 documentation. They are embedded so the
// test needs no network.
var (
	suiteCreds = awsCreds{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	suiteTime = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
)

func TestSignV4SuiteVectors(t *testing.T) {
	const scope = "AKIDEXAMPLE/20150830/us-east-1/service/aws4_request"
	cases := []struct {
		name    string
		method  string
		url     string
		headers map[string]string
		body    string
		signed  string
		sig     string
	}{
		{
			name:   "get-vanilla",
			method: "GET", url: "https://example.amazonaws.com/",
			signed: "host;x-amz-date",
			sig:    "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name:   "post-vanilla",
			method: "POST", url: "https://example.amazonaws.com/",
			signed: "host;x-amz-date",
			sig:    "5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b",
		},
		{
			name:   "get-vanilla-query-order-key-case",
			method: "GET", url: "https://example.amazonaws.com/?Param2=value2&Param1=value1",
			signed: "host;x-amz-date",
			sig:    "b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500",
		},
		{
			name:   "post-x-www-form-urlencoded",
			method: "POST", url: "https://example.amazonaws.com/",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			body:    "Param1=value1",
			signed:  "content-type;host;x-amz-date",
			sig:     "ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(c.method, c.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			signV4(req, []byte(c.body), suiteCreds, "us-east-1", "service", suiteTime)
			want := "AWS4-HMAC-SHA256 Credential=" + scope + ", SignedHeaders=" + c.signed + ", Signature=" + c.sig
			if got := req.Header.Get("Authorization"); got != want {
				t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
			}
			if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
				t.Errorf("X-Amz-Date = %q", got)
			}
		})
	}
}

// The derived signing key from the SigV4 documentation's worked example.
func TestSigningKeyVector(t *testing.T) {
	got := hex.EncodeToString(signingKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "iam"))
	if want := "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"; got != want {
		t.Errorf("signing key = %s, want %s", got, want)
	}
}

func TestSignV4SessionTokenIsSigned(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://kms.eu-west-1.amazonaws.com/", nil)
	c := suiteCreds
	c.SessionToken = "tok/with+chars"
	signV4(req, nil, c, "eu-west-1", "kms", suiteTime)
	if got := req.Header.Get("X-Amz-Security-Token"); got != c.SessionToken {
		t.Errorf("X-Amz-Security-Token = %q", got)
	}
	if auth := req.Header.Get("Authorization"); !strings.Contains(auth, "SignedHeaders=host;x-amz-date;x-amz-security-token,") {
		t.Errorf("session token not in signed headers: %s", auth)
	}
}

func TestAWSEncode(t *testing.T) {
	for in, want := range map[string]string{
		"AZaz09-_.~": "AZaz09-_.~",
		"a b/c":      "a%20b%2Fc",
		"é":          "%C3%A9",
		"+=&":        "%2B%3D%26",
	} {
		if got := awsEncode(in); got != want {
			t.Errorf("awsEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

// The shape of a real KMS call: temporary credentials, X-Amz-Target and a
// padded header value. This is NOT a published AWS vector -- the test suite
// files for session tokens were not available offline, so none is invented
// here. It pins the canonicalisation instead: the canonical request is written
// out by hand from the SigV4 specification (sorted lower-case names, trimmed
// and collapsed values, token and target signed) and signed with the HMAC chain
// spelled out again, independently of signV4.
func TestSignV4TemporaryCredentialsKMSShape(t *testing.T) {
	body := []byte(`{"CiphertextBlob":"AQID"}`)
	req, _ := http.NewRequest("POST", "https://kms.eu-west-1.amazonaws.com/", nil)
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "  TrentService.Decrypt   ")
	req.Header.Set("User-Agent", "ignored/1.0")
	c := suiteCreds
	c.SessionToken = "tok"
	signV4(req, body, c, "eu-west-1", "kms", suiteTime)

	sum := sha256.Sum256(body)
	canonical := "POST\n/\n\n" +
		"content-type:application/x-amz-json-1.1\n" +
		"host:kms.eu-west-1.amazonaws.com\n" +
		"x-amz-date:20150830T123600Z\n" +
		"x-amz-security-token:tok\n" +
		"x-amz-target:TrentService.Decrypt\n\n" +
		"content-type;host;x-amz-date;x-amz-security-token;x-amz-target\n" +
		hex.EncodeToString(sum[:])
	cs := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/eu-west-1/kms/aws4_request\n" + hex.EncodeToString(cs[:])
	mac := func(k []byte, d string) []byte { h := hmac.New(sha256.New, k); h.Write([]byte(d)); return h.Sum(nil) }
	k := mac([]byte("AWS4"+c.SecretAccessKey), "20150830")
	k = mac(mac(mac(k, "eu-west-1"), "kms"), "aws4_request")
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/eu-west-1/kms/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date;x-amz-security-token;x-amz-target, Signature=" +
		hex.EncodeToString(mac(k, toSign))
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
	}
}
