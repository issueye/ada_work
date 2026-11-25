package colorval

import "testing"

func TestValidHex(t *testing.T) {
	valid := []string{"#abc", "#abcd", "#112233", "#11223344", "#ABCDEF", "#000"}
	for _, s := range valid {
		if !ValidHex(s) {
			t.Errorf("ValidHex(%q) = false", s)
		}
	}
	invalid := []string{"", "#", "#12", "#12345", "#1234567", "#ggg", "abc", "#12 3", "#123456789", " #abc"}
	for _, s := range invalid {
		if ValidHex(s) {
			t.Errorf("ValidHex(%q) = true", s)
		}
	}
}
