package provider

import "regexp"

// credentialPatterns match strings that look like API keys or bearer tokens
// so an upstream that echoes its credential back in an error message does
// not leak it into Ragmux's logs, request log or client responses.
//
// The bearer pattern runs first so "Bearer sk-..." collapses into a single
// placeholder instead of leaving the scheme behind.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`Bearer [A-Za-z0-9._-]{8,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`),
}

// Redacted replaces every match of the credential patterns.
const Redacted = "[redacted]"

// Redact masks anything that looks like a credential in s.
func Redact(s string) string {
	for _, re := range credentialPatterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}
