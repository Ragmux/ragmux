package store

// Credential prefixes. All three keep the "sk-" head so the secret scanners
// that already watch for OpenAI-shaped keys keep firing on a leak.
const (
	// ProjectKeyPrefix marks a project's default key: the one key stored on
	// the project row itself, without an owner and only rotatable.
	ProjectKeyPrefix = "sk-proj-"
	// GatewayKeyPrefix marks a user-owned /v1 key.
	GatewayKeyPrefix = "sk-user-"
	// ManagementKeyPrefix marks a user-owned /admin/api key.
	ManagementKeyPrefix = "sk-mgmt-"
)

// keyBodyLen is the random part of every credential, in keyAlphabet
// characters: 43 of 62 symbols is just over 256 bits.
const keyBodyLen = 43

// Key kinds stored in api_keys.kind.
const (
	KindGateway    = "gateway"
	KindManagement = "management"
)

// Scopes a gateway key may carry. They name the two /v1 route groups that
// exist; there are deliberately no scopes for endpoints Ragmux does not have.
const (
	ScopeChat   = "chat"
	ScopeModels = "models"
)

// Scopes a management key may carry. They mirror the role matrix of
// /admin/api: read for the listings, write for the editor routes, admin for
// the admin-only ones. Minting keys is separate from write so a leaked write
// key cannot issue more keys.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
	ScopeAdmin = "admin"
	ScopeKeys  = "keys"
)

// The scope lists are functions rather than package-level slices on purpose.
// A shared slice is one append or index assignment away from changing what
// every future key in the process gets, and the caller that did it would be
// nowhere near this file. Handing out a fresh slice each time costs one tiny
// allocation on a path that already talks to the database.

// GatewayScopes lists the scopes a gateway key may carry.
func GatewayScopes() []string { return []string{ScopeChat, ScopeModels} }

// ManagementScopes lists the scopes a management key may carry.
func ManagementScopes() []string { return []string{ScopeRead, ScopeWrite, ScopeAdmin, ScopeKeys} }

// DefaultGatewayScopes is what a gateway key gets when none are given: every
// scope a gateway key can hold, since there are only the two.
func DefaultGatewayScopes() []string { return GatewayScopes() }

// ValidScopes returns the scopes a key of this kind may carry.
func ValidScopes(kind string) []string {
	if kind == KindManagement {
		return ManagementScopes()
	}
	return GatewayScopes()
}

// KeyPrefixFor returns the credential prefix of a key kind.
func KeyPrefixFor(kind string) string {
	if kind == KindManagement {
		return ManagementKeyPrefix
	}
	return GatewayKeyPrefix
}

// GenerateAPIKey produces a fresh credential for a key kind.
func GenerateAPIKey(kind string) (string, error) {
	return generateToken(KeyPrefixFor(kind), keyBodyLen)
}

// GeneratePassword produces a random password of n characters from the same
// alphabet, for the reset-password CLI. It has no punctuation, so it survives
// being pasted through a shell or a password manager unchanged.
func GeneratePassword(n int) (string, error) {
	return generateToken("", n)
}
