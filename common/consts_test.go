package common

import (
	"strings"
	"testing"
)

func TestValidMetricName(t *testing.T) {
	if !ValidMetricName(strings.Repeat("m", MaxMetricNameLen)) || ValidMetricName(strings.Repeat("m", MaxMetricNameLen+1)) {
		t.Error("metric names follow the OpenTelemetry 255 limit, not the 200 service/host cap")
	}
	if ValidMetricName("") || ValidMetricName("a\x00b") {
		t.Error("empty and control characters must be refused")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"api", "payment-service", "host.example.com", "svc_1/eu", "сервис", strings.Repeat("x", MaxNameLen)} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false, want true", ok)
		}
	}
	for name, bad := range map[string]string{
		"empty":        "",
		"too long":     strings.Repeat("x", MaxNameLen+1),
		"NUL":          "api\x00evil",
		"newline":      "api\nINFO forged line",
		"carriage ret": "api\r",
		"tab":          "a\tb",
		"escape":       "a\x1b[31m",
		"DEL":          "a\x7f",
		"C1 control":   "a\u0085b",
		"invalid utf8": "a\xffb",
	} {
		if ValidName(bad) {
			t.Errorf("%s: ValidName(%q) = true, want false", name, bad)
		}
	}
}
