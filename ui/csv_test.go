package ui

import "testing"

func TestCSVSafe(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"payment declined", "payment declined"},
		{"=HYPERLINK(\"https://evil\",\"x\")", "'=HYPERLINK(\"https://evil\",\"x\")"},
		{"+1234567890", "+1234567890"}, // a positive number literal, harmless
		{"=1+1", "'=1+1"},              // = always a formula, never a number
		{"-2+cmd|'/c calc'!A1", "'-2+cmd|'/c calc'!A1"},
		{"-2+3", "'-2+3"}, // a formula, not a number
		{"@SUM(A1:A9)", "'@SUM(A1:A9)"},
		{"\tleading tab", "'\tleading tab"},
		{"\rleading cr", "'\rleading cr"},
		{"1.5", "1.5"},   // a bare number is not a formula trigger
		{"-1.5", "-1.5"}, // negative numbers stay numeric
		{"+3", "+3"},
	}
	for _, tt := range tests {
		if got := csvSafe(tt.in); got != tt.want {
			t.Errorf("csvSafe(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
