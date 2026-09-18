package provider

import (
	"regexp"
	"strings"
)

// credentialPatterns match strings that look like API keys or bearer tokens
// so an upstream that echoes its credential back in an error message does
// not leak it into Ragmux's logs, request log or client responses.
//
// The bearer pattern runs first so "Bearer sk-..." collapses into a single
// placeholder instead of leaving the scheme behind; the URL userinfo pattern
// keeps the "://" and "@" so the address stays readable.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`Bearer [A-Za-z0-9._-]{8,}`),
	regexp.MustCompile(`Basic [A-Za-z0-9+/=]{8,}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_-]+)?`), // JWT
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`),
	regexp.MustCompile(`ya29\.[A-Za-z0-9_-]{10,}`), // Google OAuth access token
	regexp.MustCompile(`(?:gsk|hf|xai)[_-][A-Za-z0-9]{8,}`),
}

// userinfoPattern matches credentials embedded in a URL (scheme://user:pass@host).
var userinfoPattern = regexp.MustCompile(`://[^/\s:@]+:[^@/\s]+@`)

// Redacted replaces every match of the credential patterns.
const Redacted = "[redacted]"

// Redact masks anything that looks like a credential in s.
func Redact(s string) string {
	s = userinfoPattern.ReplaceAllString(s, "://"+Redacted+"@")
	for _, re := range credentialPatterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}

// RedactWith is Redact plus a literal replacement of every non-empty secret,
// for credentials whose shape no pattern knows (a random custom_openai key,
// for instance).
func RedactWith(msg string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			msg = strings.ReplaceAll(msg, sec, Redacted)
		}
	}
	return Redact(msg)
}
