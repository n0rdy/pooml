package query

import "testing"

func TestFTSMatch(t *testing.T) {
	tests := []struct{ in, want string }{
		{"order", `"order"*`},
		{"order 502", `"order"* "502"*`},
		{"  padded  ", `"padded"*`},
		{"u_991:", `"u_991:"*`},
		{"", ""},
		// power-user syntax passes through untouched
		{`"payment failed"`, `"payment failed"`},
		{"err* gateway", "err* gateway"},
		{"payment AND gateway", "payment AND gateway"},
		{"payment NOT refund", "payment NOT refund"},
		{"NEAR(a b)", "NEAR(a b)"},
		{"^first", "^first"},
		// lowercase and/or are just words, not operators
		{"and or not", `"and"* "or"* "not"*`},
	}
	for _, tt := range tests {
		if got := FTSMatch(tt.in); got != tt.want {
			t.Errorf("FTSMatch(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
