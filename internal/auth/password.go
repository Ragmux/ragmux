package auth

import (
	"fmt"
	"unicode/utf8"
)

// Password policy. It is the single source for every place a password is
// set: user management, the first-run setup, `ragmux reset-password` and the
// ADMIN_PASSWORD bootstrap. Only the length is checked; existing passwords
// are never re-validated.
const (
	// MinPasswordLen is the shortest acceptable password, in characters.
	MinPasswordLen = 12
	// MaxPasswordLen is bcrypt's input limit in bytes; longer passwords are
	// refused rather than silently truncated.
	MaxPasswordLen = 72
)

// Errors returned by ValidatePassword. Their messages are shown to users
// as-is.
var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d bytes", MaxPasswordLen)
)

// ValidatePassword reports whether pw may be set as a new password. The
// minimum counts characters, so a multi-byte password is not refused for
// being short when it is not; the maximum counts bytes, as bcrypt does.
func ValidatePassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	if len(pw) > MaxPasswordLen {
		return ErrPasswordTooLong
	}
	return nil
}
