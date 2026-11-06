package auth

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B: the SHA-1 vectors (secret "12345678901234567890", eight
// digits). hotp is the whole algorithm; the six-digit code the app shows is the
// same number taken modulo 10^6.
func TestTOTPRFC6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")
	for _, v := range []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		got := hotp(key, uint64(totpStep(time.Unix(v.unix, 0))), 8)
		if got != v.want {
			t.Errorf("T=%d: %s, want %s", v.unix, got, v.want)
		}
		// Six digits are the last six of the eight.
		if six := hotp(key, uint64(totpStep(time.Unix(v.unix, 0))), 6); six != v.want[2:] {
			t.Errorf("T=%d: six digits %s, want %s", v.unix, six, v.want[2:])
		}
	}
}

func TestVerifyTOTPWindowAndReplay(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 32 || strings.ContainsAny(secret, "=189") {
		t.Errorf("secret %q: want 32 unpadded base32 characters (20 bytes)", secret)
	}
	now := time.Unix(1_800_000_000, 0)
	at := func(d int) string {
		c, err := TOTPCodeAt(secret, now.Add(time.Duration(d)*TOTPPeriod*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cur := totpStep(now)

	if step, ok := VerifyTOTP(secret, at(0), 0, now); !ok || step != cur {
		t.Fatalf("current code: step=%d ok=%v, want %d", step, ok, cur)
	}
	// One step either side is allowed for clock drift ...
	if step, ok := VerifyTOTP(secret, at(-1), 0, now); !ok || step != cur-1 {
		t.Errorf("previous step: %d %v", step, ok)
	}
	if step, ok := VerifyTOTP(secret, at(1), 0, now); !ok || step != cur+1 {
		t.Errorf("next step: %d %v", step, ok)
	}
	// ... two are not.
	for _, d := range []int{-2, 2} {
		if _, ok := VerifyTOTP(secret, at(d), 0, now); ok {
			t.Errorf("a code %d steps away was accepted", d)
		}
	}
	// A step that was already accepted, or an older one, is refused: the same
	// code twice, and a code from before the last one.
	if _, ok := VerifyTOTP(secret, at(0), cur, now); ok {
		t.Error("the step that was just accepted passed a second time")
	}
	if _, ok := VerifyTOTP(secret, at(-1), cur, now); ok {
		t.Error("a step older than the last accepted one passed")
	}
	// The next step is still fine after the current one was used.
	if step, ok := VerifyTOTP(secret, at(1), cur, now); !ok || step != cur+1 {
		t.Errorf("the next step after a used one: %d %v", step, ok)
	}
	// Formatting: spaces are ignored, anything else that is not six digits fails.
	c := at(0)
	if _, ok := VerifyTOTP(secret, c[:3]+" "+c[3:], 0, now); !ok {
		t.Error("a code typed as '123 456' was refused")
	}
	for _, bad := range []string{"", "12345", "1234567", "12345a", "١٢٣٤٥٦", strings.Repeat("0", 100)} {
		if _, ok := VerifyTOTP(secret, bad, 0, now); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
	if _, ok := VerifyTOTP("not base32!", at(0), 0, now); ok {
		t.Error("a malformed secret verified")
	}
}

func TestTOTPURI(t *testing.T) {
	got := TOTPURI("ali", "JBSWY3DPEHPK3PXP")
	if got != "otpauth://totp/Ragmux:ali?secret=JBSWY3DPEHPK3PXP&issuer=Ragmux" {
		t.Errorf("uri = %q", got)
	}
	if got := TOTPURI("a b/c", "X"); !strings.HasPrefix(got, "otpauth://totp/Ragmux:a%20b%2Fc?") {
		t.Errorf("a name with a space and a slash must be escaped: %q", got)
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashes, err := GenerateRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 10 || len(hashes) != 10 {
		t.Fatalf("got %d codes and %d hashes", len(codes), len(hashes))
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if len(c) != 11 || c[5] != '-' || strings.Trim(c, crockford+"-") != "" {
			t.Errorf("code %q is not xxxxx-xxxxx over the Crockford alphabet", c)
		}
		norm, ok := NormalizeRecoveryCode(c)
		if !ok || HashRecoveryCode(norm) != hashes[i] {
			t.Errorf("code %q does not normalise back to its hash", c)
		}
		if seen[c] {
			t.Errorf("code %q was generated twice", c)
		}
		seen[c] = true
		if strings.Contains(hashes[i], strings.ReplaceAll(c, "-", "")) {
			t.Errorf("the hash of %q contains the code", c)
		}
	}
	// How people type them: lower case, no dash, spaces, O for 0, I and L for 1.
	for in, want := range map[string]string{
		"abcde-fghjk": "ABCDEFGHJK",
		"ABCDEFGHJK":  "ABCDEFGHJK",
		"abcde fghjk": "ABCDEFGHJK",
		"OOIIL-00112": "0011100112",
	} {
		if got, ok := NormalizeRecoveryCode(in); !ok || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "abcde", "abcde-fghjk0", "abcde-fghju", "abcde-fgh!k", "ÄBCDE-FGHJK"} {
		if _, ok := NormalizeRecoveryCode(bad); ok {
			t.Errorf("%q was taken for a recovery code", bad)
		}
	}
}
