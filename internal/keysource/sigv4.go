package keysource

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// awsCreds are the credentials a request is signed with.
type awsCreds struct {
	AccessKeyID     string
	SecretAccessKey string
	// SessionToken is set for temporary credentials.
	SessionToken string
}

// sigV4Algorithm is the only signing algorithm this file implements.
const sigV4Algorithm = "AWS4-HMAC-SHA256"

// unsignedHeaders never enter the signature: a proxy or the transport may
// rewrite or add them after signing.
var unsignedHeaders = map[string]bool{
	"authorization":   true,
	"user-agent":      true,
	"x-amzn-trace-id": true,
	"expect":          true,
	"connection":      true,
}

// signV4 signs req in place with AWS Signature Version 4: it sets X-Amz-Date
// (and X-Amz-Security-Token for temporary credentials) and the Authorization
// header. body is the exact payload that will be sent; every header already on
// req, plus Host, is signed.
//
// It is the non-S3 variant: the path is encoded twice. Dot segments are not
// normalised because the only paths signed here are fixed ("/").
func signV4(req *http.Request, body []byte, c awsCreds, region, service string, t time.Time) {
	t = t.UTC()
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		if unsignedHeaders[lower] {
			continue
		}
		trimmed := make([]string, len(vals))
		for i, v := range vals {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		headers[lower] = strings.Join(trimmed, ",")
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + headers[n] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonical := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL.Path),
		canonicalQuery(req),
		canonHeaders.String(),
		signedHeaders,
		hexSHA256(body),
	}, "\n")

	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := strings.Join([]string{sigV4Algorithm, amzDate, scope, hexSHA256([]byte(canonical))}, "\n")
	sig := hex.EncodeToString(hmacSHA256(signingKey(c.SecretAccessKey, date, region, service), toSign))
	req.Header.Set("Authorization", sigV4Algorithm+" Credential="+c.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
}

// signingKey derives the per-day, per-region, per-service key.
func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// canonicalPath double-encodes each segment of the (decoded) path.
func canonicalPath(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsEncode(awsEncode(s))
	}
	out := strings.Join(segs, "/")
	if !strings.HasPrefix(out, "/") {
		out = "/" + out
	}
	return out
}

// canonicalQuery sorts the encoded parameters by name, then value.
func canonicalQuery(req *http.Request) string {
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range req.URL.Query() {
		for _, v := range vs {
			pairs = append(pairs, kv{awsEncode(k), awsEncode(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// awsEncode is the RFC 3986 percent-encoding SigV4 asks for: everything but
// A-Z a-z 0-9 - _ . ~ is escaped, with upper-case hex.
func awsEncode(s string) string {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&15])
		}
	}
	return b.String()
}
