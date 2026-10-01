package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		want error
	}{
		{"empty", "", ErrPasswordTooShort},
		{"11 characters", strings.Repeat("a", 11), ErrPasswordTooShort},
		{"12 characters", strings.Repeat("a", 12), nil},
		{"72 bytes", strings.Repeat("a", 72), nil},
		{"73 bytes", strings.Repeat("a", 73), ErrPasswordTooLong},
		// 12 two-byte characters: 24 bytes, accepted on the character count.
		{"12 multi-byte characters", strings.Repeat("ş", 12), nil},
		// 11 two-byte characters are 22 bytes but still too short.
		{"11 multi-byte characters", strings.Repeat("ş", 11), ErrPasswordTooShort},
		// 37 two-byte characters fit the character floor but not bcrypt's limit.
		{"74 bytes of multi-byte characters", strings.Repeat("ş", 37), ErrPasswordTooLong},
	}
	for _, c := range cases {
		if got := ValidatePassword(c.pw); !errors.Is(got, c.want) {
			t.Errorf("%s: ValidatePassword = %v, want %v", c.name, got, c.want)
		}
	}
	// The messages are part of the API: every entry point returns them verbatim.
	if got := ErrPasswordTooShort.Error(); got != "password must be at least 12 characters" {
		t.Errorf("too-short message: %q", got)
	}
	if got := ErrPasswordTooLong.Error(); got != "password must be at most 72 bytes" {
		t.Errorf("too-long message: %q", got)
	}
}
