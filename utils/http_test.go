package utils

import (
	"net/http"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		trust      bool
		remoteAddr string
		xff        []string // one entry per header field line
		want       string
	}{
		{name: "no trust ignores xff", trust: false, remoteAddr: "203.0.113.9:5555",
			xff: []string{"1.2.3.4"}, want: "203.0.113.9"},
		{name: "trust single appended line takes rightmost", trust: true, remoteAddr: "10.0.0.1:1",
			xff: []string{"1.2.3.4, 203.0.113.9"}, want: "203.0.113.9"},
		{name: "trust separate proxy line takes rightmost across lines", trust: true, remoteAddr: "10.0.0.1:1",
			xff: []string{"1.2.3.4", "203.0.113.9"}, want: "203.0.113.9"},
		{name: "trust falls back to remoteaddr when xff absent", trust: true, remoteAddr: "203.0.113.9:5555",
			want: "203.0.113.9"},
		{name: "trust skips a malformed rightmost entry", trust: true, remoteAddr: "10.0.0.1:1",
			xff: []string{"203.0.113.9, not-an-ip"}, want: "203.0.113.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &http.Request{RemoteAddr: tt.remoteAddr, Header: http.Header{}}
			for _, line := range tt.xff {
				req.Header.Add("X-Forwarded-For", line)
			}
			if got := ClientIP(req, tt.trust); got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}
