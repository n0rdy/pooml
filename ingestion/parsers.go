package ingestion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/n0rdy/pooml/common"
)

var (
	// Combined Log Format with the referer/user-agent tail optional, so plain
	// CLF (nginx default, Traefik) matches too.
	clfRe = regexp.MustCompile(
		`^(\S+) \S+ (\S+) \[([^\]]+)\] "([^"]*)" (\d{3}) (\S+)(?:\s+"([^"]*)"\s+"([^"]*)")?`)

	plainLevelRe = regexp.MustCompile(`(?i)\b(trace|debug|info|warn|warning|error|fatal|panic|critical)\b`)
)

var levelNames = map[string]int{
	"trace": common.LevelTrace,
	"debug": common.LevelDebug,
	"info":  common.LevelInfo, "information": common.LevelInfo,
	"warn": common.LevelWarn, "warning": common.LevelWarn,
	"error": common.LevelError, "err": common.LevelError,
	"fatal": common.LevelFatal, "panic": common.LevelFatal, "critical": common.LevelFatal,
}

var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
}

// parsePayload turns one HTTP payload into 0..N normalized logs. A FluentBit
// envelope is unwrapped whole; anything else is parsed line by line.
func parsePayload(service, host string, payload []byte, receivedAt int64) []common.StandardLog {
	if logs, ok := parseFluentBit(service, host, payload, receivedAt); ok {
		return logs
	}

	var out []common.StandardLog
	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		l := parseLine(line)
		l.Service, l.Host, l.IngestedAt = service, host, receivedAt
		if l.Timestamp == 0 {
			l.Timestamp = receivedAt
		}
		out = append(out, l)
	}
	return out
}

// parseLine never fails: the plain-text parser is a universal fallback.
// Timestamp is left 0 when the line carries none; callers fill the fallback.
func parseLine(line []byte) common.StandardLog {
	if l, ok := parseJSONLine(line); ok {
		return l
	}
	if l, ok := parseCLFLine(line); ok {
		return l
	}
	return parsePlainLine(line)
}

func parseJSONLine(line []byte) (common.StandardLog, bool) {
	if line[0] != '{' {
		return common.StandardLog{}, false
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return common.StandardLog{}, false
	}

	raw := string(line)
	l := common.StandardLog{Raw: raw, Parsed: &raw}

	for _, k := range []string{"level", "lvl", "severity"} {
		if v, ok := m[k]; ok {
			if lv := levelFrom(v); lv != nil {
				l.Level = lv
				break
			}
		}
	}
	for _, k := range []string{"ts", "time", "timestamp", "@timestamp", "date"} {
		if v, ok := m[k]; ok {
			if ts := tsFrom(v); ts != 0 {
				l.Timestamp = ts
				break
			}
		}
	}
	for _, k := range []string{"msg", "message"} {
		if s, ok := m[k].(string); ok && s != "" {
			l.Message = &s
			break
		}
	}
	return l, true
}

func parseCLFLine(line []byte) (common.StandardLog, bool) {
	m := clfRe.FindSubmatch(line)
	if m == nil {
		return common.StandardLog{}, false
	}

	status, _ := strconv.Atoi(string(m[5]))
	lvl := common.LevelInfo
	switch {
	case status >= 500:
		lvl = common.LevelError
	case status >= 400:
		lvl = common.LevelWarn
	}

	l := common.StandardLog{Raw: string(line), Level: &lvl}
	if t, err := time.Parse("02/Jan/2006:15:04:05 -0700", string(m[3])); err == nil {
		l.Timestamp = t.UnixMilli()
	}

	method, path, proto := splitRequest(string(m[4]))
	msg := fmt.Sprintf("%s %s %d", method, path, status)
	l.Message = &msg

	parsed := map[string]any{"ip": string(m[1]), "method": method, "path": path, "status": status}
	if proto != "" {
		parsed["proto"] = proto
	}
	if user := string(m[2]); user != "-" && user != "" {
		parsed["user"] = user
	}
	if b, err := strconv.Atoi(string(m[6])); err == nil {
		parsed["bytes"] = b
	}
	if ref := string(m[7]); ref != "" && ref != "-" {
		parsed["referer"] = ref
	}
	if ua := string(m[8]); ua != "" && ua != "-" {
		parsed["user_agent"] = ua
	}
	if pj, err := json.Marshal(parsed); err == nil {
		s := string(pj)
		l.Parsed = &s
	}
	return l, true
}

func parsePlainLine(line []byte) common.StandardLog {
	s := string(line)
	l := common.StandardLog{Raw: s, Message: &s}
	if m := plainLevelRe.FindString(s); m != "" {
		if lv, ok := levelNames[strings.ToLower(m)]; ok {
			l.Level = &lv
		}
	}
	l.Timestamp = leadingTimestamp(s)
	return l
}

func splitRequest(req string) (method, path, proto string) {
	parts := strings.SplitN(req, " ", 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], parts[1], ""
	default:
		return req, "", ""
	}
}

func levelFrom(v any) *int {
	switch t := v.(type) {
	case string:
		if lv, ok := levelNames[strings.ToLower(t)]; ok {
			return &lv
		}
	case float64:
		n := int(t)
		if n >= 0 && n <= 5 {
			return &n
		}
		// bunyan numeric levels: 10=trace .. 60=fatal
		if n >= 10 && n <= 60 && n%10 == 0 {
			n = n/10 - 1
			return &n
		}
	}
	return nil
}

func tsFrom(v any) int64 {
	switch t := v.(type) {
	case float64:
		return toMs(t)
	case string:
		return parseTimeString(t)
	}
	return 0
}

// toMs normalizes an epoch of unknown unit (s/ms/µs/ns) to milliseconds.
func toMs(v float64) int64 {
	switch {
	case v <= 0:
		return 0
	case v < 1e11:
		return int64(v * 1000)
	case v < 1e14:
		return int64(v)
	case v < 1e17:
		return int64(v / 1e3)
	default:
		return int64(v / 1e6)
	}
}

func parseTimeString(s string) int64 {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// leadingTimestamp best-effort parses a timestamp from the first one or two
// whitespace-separated tokens of a plain-text line.
func leadingTimestamp(s string) int64 {
	first, rest, _ := strings.Cut(s, " ")
	if ts := parseTimeString(first); ts != 0 {
		return ts
	}
	if second, _, _ := strings.Cut(rest, " "); second != "" {
		return parseTimeString(first + " " + second)
	}
	return 0
}
