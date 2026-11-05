package guardrail

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidTCKN(t *testing.T) {
	for _, ok := range []string{"10000000146", "11111111110", "12345678950"} {
		if !ValidTCKN(ok) {
			t.Errorf("ValidTCKN(%q) = false, want true", ok)
		}
	}
	for name, bad := range map[string]string{
		"wrong tenth digit":    "10000000246",
		"wrong eleventh digit": "10000000147",
		"leading zero":         "01000000146",
		"too short":            "1000000014",
		"too long":             "100000001460",
		"letters":              "1000000014a",
		"spaces":               "1000 000 0146",
		"empty":                "",
		"all zero":             "00000000000",
		"non-ascii digits":     "١٠٠٠٠٠٠٠١٤٦",
	} {
		if ValidTCKN(bad) {
			t.Errorf("%s: ValidTCKN(%q) = true, want false", name, bad)
		}
	}
}

func TestFindTCKN(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"alone", "10000000146", []string{"10000000146"}},
		{"in a sentence", "TC kimlik no: 10000000146, teşekkürler", []string{"10000000146"}},
		{"two of them", "10000000146 ve 11111111110", []string{"10000000146", "11111111110"}},
		{"a digit before", "910000000146", nil},
		{"a digit after", "100000001467", nil},
		{"a longer number that contains one", "9910000000146", nil},
		{"invalid checksum", "10000000147", nil},
		{"leading zero", "01234567890", nil},
		{"letters stick to it", "x10000000146y", []string{"10000000146"}},
		{"no digits", "hello world", nil},
	}
	for _, tc := range cases {
		var got []string
		for _, sp := range findTCKN(tc.in) {
			got = append(got, tc.in[sp.Start:sp.End])
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: findTCKN(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestValidIBAN(t *testing.T) {
	for _, ok := range []string{
		"TR330006100519786457841326",
		"TR33 0006 1005 1978 6457 8413 26",
		"tr33 0006 1005 1978 6457 8413 26",
		"GB82WEST12345698765432",
		"GB82 WEST 1234 5698 7654 32",
		"DE89370400440532013000",
		"DE89 3704 0044 0532 0130 00",
		"FR1420041010050500013M02606",
		"NL91ABNA0417164300",
	} {
		if !ValidIBAN(ok) {
			t.Errorf("ValidIBAN(%q) = false, want true", ok)
		}
	}
	for name, bad := range map[string]string{
		"wrong check digits": "TR340006100519786457841326",
		"one digit off":      "TR330006100519786457841327",
		"wrong length":       "TR33000610051978645784132",
		"unknown country":    "ZZ330006100519786457841326",
		"digits as country":  "12330006100519786457841326",
		"punctuation":        "TR33-0006-1005-1978-6457-8413-26",
		"empty":              "",
		"short":              "TR33",
	} {
		if ValidIBAN(bad) {
			t.Errorf("%s: ValidIBAN(%q) = true, want false", name, bad)
		}
	}
}

func TestFindIBAN(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"compact", "TR330006100519786457841326", []string{"TR330006100519786457841326"}},
		{"grouped", "IBAN: TR33 0006 1005 1978 6457 8413 26.", []string{"TR33 0006 1005 1978 6457 8413 26"}},
		{"lower case", "tr33 0006 1005 1978 6457 8413 26", []string{"tr33 0006 1005 1978 6457 8413 26"}},
		{"german", "DE89 3704 0044 0532 0130 00 ve GB82WEST12345698765432", []string{"DE89 3704 0044 0532 0130 00", "GB82WEST12345698765432"}},
		{"a letter before", "xTR330006100519786457841326", nil},
		{"a digit after", "TR3300061005197864578413261", nil},
		{"a letter after", "TR330006100519786457841326x", nil},
		{"wrong checksum", "TR340006100519786457841326", nil},
		{"too short", "TR33 0006 1005 1978 6457 8413", nil},
		{"two spaces", "TR33  0006 1005 1978 6457 8413 26", nil},
		{"a space inside a group", "TR33 00 06 1005 1978 6457 8413 26", nil},
		{"unknown country", "ZZ330006100519786457841326", nil},
		{"trailing space is not part of it", "TR330006100519786457841326 ", []string{"TR330006100519786457841326"}},
	}
	for _, tc := range cases {
		var got []string
		for _, sp := range findIBAN(tc.in) {
			got = append(got, tc.in[sp.Start:sp.End])
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: findIBAN(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// ibanFor builds a valid IBAN of the country's registered length by computing
// the check digits of a made-up account number.
func ibanFor(t *testing.T, country string) string {
	t.Helper()
	n, ok := ibanLengths[country]
	if !ok {
		t.Fatalf("no length for %s", country)
	}
	bban := strings.Repeat("1234567890", 4)[:n-4]
	rem := 0
	for _, c := range bban + country + "00" {
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		default:
			rem = (rem*100 + int(c-'A') + 10) % 97
		}
	}
	return fmt.Sprintf("%s%02d%s", country, 98-rem, bban)
}

// The countries the registry added after the first table was written still
// count as IBANs; one missing from the table is an IBAN that passes unseen.
func TestIBANRegistryCountries(t *testing.T) {
	for _, cc := range []string{"RU", "OM", "YE", "SO", "MN", "NI", "HN", "FK", "DJ", "BI", "LC", "TR", "DE"} {
		v := ibanFor(t, cc)
		if !ValidIBAN(v) {
			t.Errorf("%s: ValidIBAN(%q) = false", cc, v)
		}
		if got := findIBAN("pay " + v + " now"); len(got) != 1 {
			t.Errorf("%s: findIBAN found %v in a text holding %q", cc, got, v)
		}
		if ValidIBAN(v + "0") {
			t.Errorf("%s: an IBAN one character too long was accepted", cc)
		}
	}
	if got := ibanFor(t, "RU"); len(got) != 33 {
		t.Errorf("RU IBAN has %d characters", len(got))
	}
}

// Registry examples from outside the table, so a wrong length in the table
// fails here instead of passing against itself.
func TestIBANKnownExamples(t *testing.T) {
	for _, ex := range []string{
		"GB82WEST12345698765432", "DE89370400440532013000", "FR1420041010050500013M02606", "NL91ABNA0417164300",
		"NO9386011117947", "MT84MALT011000012345MTLCAST001S", "TR330006100519786457841326", "IT60X0542811101000000123456",
		"ES9121000418450200051332", "CH9300762011623852957", "SA0380000000608010167519", "LC55HEMM000100010012001200023015",
		"BI4210000100010000332045181", "DJ2100010000000154000100186", "FK88SC123456789012", "OM810180000001299123456",
		"RU0304452522540817810538091310419", "YE15CBYE0001018861234567891234", "SO211000001001000100141",
		"MN121234123456789123", "NI45BAPR00000013000003558124", "HN88CABF00000000000250005469",
	} {
		if !ValidIBAN(ex) {
			t.Errorf("ValidIBAN(%q) = false", ex)
		}
		if want := len(ex); ibanLengths[ex[:2]] != want {
			t.Errorf("%s: table says %d, example has %d", ex[:2], ibanLengths[ex[:2]], want)
		}
	}
}
