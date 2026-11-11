package common

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// envs:
	LocalEnv = "local"
	ProEnv   = "pro"

	// log levels:
	LevelTrace = 0
	LevelDebug = 1
	LevelInfo  = 2
	LevelWarn  = 3
	LevelError = 4
	LevelFatal = 5

	// Service, host and metric names repeat on every stored row and index
	// entry, so an uncapped one turns a single request into gigabytes on disk.
	MaxNameLen = 200

	// OpenTelemetry allows instrument names up to 255 characters
	MaxMetricNameLen = 255
)

// ValidName gates service, host and metric names on every ingest path. Control
// characters are refused because names are interpolated into DSL-compiled SQL
// text, where a NUL truncates the statement, and they forge look-alike names.
func ValidName(s string) bool { return validName(s, MaxNameLen) }

func ValidMetricName(s string) bool { return validName(s, MaxMetricNameLen) }

func validName(s string, maxLen int) bool {
	if s == "" || len(s) > maxLen || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

var (
	SupportedEnvs = map[string]bool{
		LocalEnv: true,
		ProEnv:   true,
	}
)
