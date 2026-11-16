package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: RFC 6238 specifies HMAC-SHA1 and every authenticator app speaks it; it is a MAC here, not a collision-resistant hash
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP parameters (RFC 6238, ADR-037). They are the defaults every
// authenticator app assumes, which is why none of them is configurable.
const (
	// TOTPDigits is the length of a code.
	TOTPDigits = 6
	// TOTPPeriod is the length of one time step in seconds.
	TOTPPeriod = 30
	// TOTPSkew is how many steps either side of the current one are accepted,
	// for clock drift between the server and the phone.
	TOTPSkew = 1
	// totpSecretBytes is the length of a secret before base32 (160 bits, the
	// size of the HMAC-SHA1 key RFC 4226 recommends).
	totpSecretBytes = 20
	// TOTPIssuer labels the account in the authenticator app.
	TOTPIssuer = "Ragmux"
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateTOTPSecret returns a new random secret as unpadded base32, the form
// authenticator apps take.
func GenerateTOTPSecret() (string, error) {
	b := make([]byte, totpSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return totpEncoding.EncodeToString(b), nil
}

// TOTPURI is the otpauth:// URI an authenticator app imports (usually from a
// QR code the client draws). The username is escaped so a name with a space or
// a slash cannot break the label.
func TOTPURI(username, secret string) string {
	return "otpauth://totp/" + url.PathEscape(TOTPIssuer+":"+username) + "?secret=" + secret + "&issuer=" + TOTPIssuer
}

// hotp is RFC 4226 over HMAC-SHA1 with the given number of digits.
func hotp(key []byte, counter uint64, digits int) string {
	mac := hmac.New(sha1.New, key) //nolint:gosec // G401: see the sha1 import
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// totpStep is the time step number of t.
func totpStep(t time.Time) int64 { return t.Unix() / TOTPPeriod }

// VerifyTOTP checks code against the base32 secret at time now. Steps within
// TOTPSkew of the current one are tried, and only a step greater than lastStep
// is accepted (RFC 6238 5.2): the code that opened the last session, or one
// older than it, is refused. It returns the accepted step, which the caller
// stores as the new lastStep in the same transaction that grants access.
func VerifyTOTP(secret, code string, lastStep int64, now time.Time) (step int64, ok bool) {
	key, err := totpEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 {
		return 0, false
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != TOTPDigits {
		return 0, false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return 0, false
		}
	}
	cur := totpStep(now)
	// Every candidate is compared, whatever the first one said, so the time
	// taken does not tell which step matched.
	for d := int64(-TOTPSkew); d <= TOTPSkew; d++ {
		s := cur + d
		if s < 0 {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hotp(key, uint64(s), TOTPDigits)), []byte(code)) == 1 && s > lastStep {
			if !ok || s > step {
				step, ok = s, true
			}
		}
	}
	return step, ok
}

// TOTPCodeAt returns the code an authenticator app shows for the base32 secret
// at time t. The server never needs it; tests and tooling that stand in for the
// app do.
func TOTPCodeAt(secret string, t time.Time) (string, error) {
	key, err := totpEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", err
	}
	step := totpStep(t)
	if step < 0 {
		return "", errors.New("auth: time before the epoch")
	}
	return hotp(key, uint64(step), TOTPDigits), nil
}

// CheckTOTP is VerifyTOTP at the current time, in the shape
// store.SecondFactor.Check takes.
func CheckTOTP(secret, code string, lastStep int64) (int64, bool) {
	return VerifyTOTP(secret, code, lastStep, time.Now())
}

// ---- recovery codes ----

const (
	// RecoveryCodeCount is how many codes a user is given.
	RecoveryCodeCount = 10
	// recoveryCodeLen is the code length without the dash: 10 characters of
	// Crockford base32, 50 bits.
	recoveryCodeLen = 10
	// crockford is Crockford's base32 alphabet: no I, L, O or U, so a code read
	// off a sheet of paper is not mistaken for another one.
	crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// GenerateRecoveryCodes returns n fresh codes in display form (xxxxx-xxxxx)
// and the hash of each, in the same order. Only the hashes are stored.
func GenerateRecoveryCodes(n int) (codes, hashes []string, err error) {
	for i := 0; i < n; i++ {
		raw := make([]byte, recoveryCodeLen)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		var sb strings.Builder
		for _, b := range raw {
			// 256 is a multiple of 32, so the modulo does not bias the pick.
			sb.WriteByte(crockford[int(b)%len(crockford)])
		}
		plain := sb.String()
		codes = append(codes, plain[:5]+"-"+plain[5:])
		hashes = append(hashes, HashRecoveryCode(plain))
	}
	return codes, hashes, nil
}

// NormalizeRecoveryCode turns what a user typed into the canonical 10
// characters: case and dashes and spaces do not matter, and the letters
// Crockford folds together (O for 0, I and L for 1) are read as the digit. It
// reports false when the result cannot be a code.
func NormalizeRecoveryCode(in string) (string, bool) {
	var sb strings.Builder
	for _, r := range strings.ToUpper(in) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if r > 127 || !strings.ContainsRune(crockford, r) {
			return "", false
		}
		sb.WriteRune(r)
	}
	if sb.Len() != recoveryCodeLen {
		return "", false
	}
	return sb.String(), true
}

// HashRecoveryCode is the stored form of a normalised recovery code: SHA-256
// as hex. A code carries 50 bits of entropy, so a slow hash would add cost to
// every check and no protection (API keys are stored the same way).
func HashRecoveryCode(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}
