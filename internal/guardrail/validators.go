package guardrail

import "strings"

// This file holds the two built-in detectors, tckn and iban. Both are hand
// written scanners rather than regular expressions: the shape of the text is
// the easy part, the checksum is what separates a TCKN from any other eleven
// digits and an IBAN from any other long word, and a checksum is not a regular
// language. Each scanner returns the spans whose checksum holds.

// isASCIIDigit reports whether b is '0'..'9'.
func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// isASCIIAlnum reports whether b is an ASCII letter or digit.
func isASCIIAlnum(b byte) bool {
	return isASCIIDigit(b) || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// ValidTCKN reports whether s is a Turkish national identity number: eleven
// ASCII digits, the first not zero, the tenth digit equal to
// ((d1+d3+d5+d7+d9)*7 - (d2+d4+d6+d8)) mod 10 and the eleventh to the sum of
// the first ten mod 10.
func ValidTCKN(s string) bool {
	if len(s) != 11 || s[0] == '0' {
		return false
	}
	var d [11]int
	for i := 0; i < 11; i++ {
		if !isASCIIDigit(s[i]) {
			return false
		}
		d[i] = int(s[i] - '0')
	}
	odd := d[0] + d[2] + d[4] + d[6] + d[8]
	even := d[1] + d[3] + d[5] + d[7]
	// Go's % keeps the sign of the dividend, so the difference is lifted
	// above zero before the modulo.
	if ((odd*7-even)%10+10)%10 != d[9] {
		return false
	}
	sum := 0
	for i := 0; i < 10; i++ {
		sum += d[i]
	}
	return sum%10 == d[10]
}

// findTCKN returns the spans of every maximal run of exactly eleven ASCII
// digits that is a valid TCKN. A run of any other length is not looked into:
// a digit before or after the eleven means the text is some longer number.
func findTCKN(s string) []Span {
	var out []Span
	for i := 0; i < len(s); {
		if !isASCIIDigit(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isASCIIDigit(s[j]) {
			j++
		}
		if j-i == 11 && ValidTCKN(s[i:j]) {
			out = append(out, Span{i, j})
		}
		i = j
	}
	return out
}

// ibanLengths is the length of an IBAN by country code, from the SWIFT IBAN
// registry (ISO 13616) as published in 2024, including the countries that
// joined late (BI, DJ, FK, HN, MN, NI, OM, RU, SO, YE). A code that is not here
// is not an IBAN country this scanner knows.
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16, "BG": 22,
	"BH": 22, "BI": 27, "BR": 29, "BY": 28, "CH": 21, "CR": 22, "CY": 28, "CZ": 24, "DE": 22,
	"DJ": 27, "DK": 18, "DO": 28, "EE": 20, "EG": 29, "ES": 24, "FI": 18, "FK": 18, "FO": 18, "FR": 27,
	"GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28, "HN": 28, "HR": 21, "HU": 28,
	"IE": 22, "IL": 23, "IQ": 23, "IS": 26, "IT": 27, "JO": 30, "KW": 30, "KZ": 20,
	"LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20, "LV": 21, "LY": 25, "MC": 27,
	"MD": 24, "ME": 22, "MK": 19, "MN": 20, "MR": 27, "MT": 31, "MU": 30, "NI": 28, "NL": 18, "NO": 15, "OM": 23,
	"PK": 24, "PL": 28, "PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22, "RU": 33, "SA": 24,
	"SC": 31, "SD": 18, "SE": 24, "SI": 19, "SK": 24, "SM": 27, "SO": 23, "ST": 25, "SV": 28,
	"TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22, "VG": 24, "XK": 20, "YE": 30,
}

// ValidIBAN reports whether s, with spaces allowed anywhere, is an IBAN: a
// known country code, two check digits, the country's length, and a remainder
// of 1 modulo 97 once the first four characters are moved to the end and the
// letters turned into the numbers 10 to 35 (ISO 7064 as used by ISO 13616).
func ValidIBAN(s string) bool {
	norm := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == ' ':
		case c >= 'a' && c <= 'z':
			norm = append(norm, c-'a'+'A')
		case isASCIIAlnum(c):
			norm = append(norm, c)
		default:
			return false
		}
	}
	return validIBANNormalized(norm)
}

// validIBANNormalized is ValidIBAN for upper-case alphanumerics only.
func validIBANNormalized(n []byte) bool {
	if len(n) < 4 {
		return false
	}
	want, ok := ibanLengths[string(n[:2])]
	if !ok || len(n) != want {
		return false
	}
	if n[0] < 'A' || n[0] > 'Z' || n[1] < 'A' || n[1] > 'Z' || !isASCIIDigit(n[2]) || !isASCIIDigit(n[3]) {
		return false
	}
	rem := 0
	feed := func(c byte) {
		if isASCIIDigit(c) {
			rem = (rem*10 + int(c-'0')) % 97
			return
		}
		v := int(c-'A') + 10 // two decimal digits
		rem = (rem*100 + v) % 97
	}
	for _, c := range n[4:] {
		feed(c)
	}
	for _, c := range n[:4] {
		feed(c)
	}
	return rem == 1
}

// maxIBANChars is the longest IBAN the format allows (the registry's longest
// today is Russia's 33, Saint Lucia's is 32; the standard's ceiling is 34) and bounds what findIBAN reads from one starting point.
const maxIBANChars = 34

// findIBAN returns the spans of the valid IBANs in s. A candidate starts at two
// letters and two digits that do not follow another letter or digit, and runs
// over letters and digits; a single space is tolerated after each complete
// group of four, which is how an IBAN is printed. The span ends at the last
// character of the length the country code asks for, and only counts when the
// character after it is not a letter or digit.
func findIBAN(s string) []Span {
	var out []Span
	for i := 0; i+4 <= len(s); i++ {
		if !isASCIIAlnum(s[i]) || (i > 0 && isASCIIAlnum(s[i-1])) {
			continue
		}
		if !isLetter(s[i]) || !isLetter(s[i+1]) || !isASCIIDigit(s[i+2]) || !isASCIIDigit(s[i+3]) {
			continue
		}
		want, ok := ibanLengths[strings.ToUpper(s[i:i+2])]
		if !ok {
			continue
		}
		norm := make([]byte, 0, maxIBANChars)
		end := -1
		for j := i; j < len(s) && len(norm) < want; j++ {
			c := s[j]
			switch {
			case isASCIIAlnum(c):
				if c >= 'a' && c <= 'z' {
					c = c - 'a' + 'A'
				}
				norm = append(norm, c)
				if len(norm) == want {
					end = j + 1
				}
			case c == ' ' && len(norm)%4 == 0 && j+1 < len(s) && isASCIIAlnum(s[j+1]):
				// A group boundary; the space is not part of the IBAN.
			default:
				j = len(s) // not an IBAN character: stop reading this candidate
			}
		}
		if end < 0 || (end < len(s) && isASCIIAlnum(s[end])) || !validIBANNormalized(norm) {
			continue
		}
		out = append(out, Span{i, end})
		i = end - 1
	}
	return out
}

// isLetter reports whether b is an ASCII letter.
func isLetter(b byte) bool { return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') }
