package ingestion

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/n0rdy/pooml/common"
)

// parseFluentBit detects and unwraps a FluentBit HTTP-output envelope
// (Format json): a JSON array of records where each carries "log" and/or
// "date". The inner log line goes through the normal parser chain; envelope
// metadata fills what the line itself doesn't provide.
func parseFluentBit(service, host string, payload []byte, receivedAt int64) ([]common.StandardLog, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var records []map[string]any
	if err := json.Unmarshal(trimmed, &records); err != nil || len(records) == 0 {
		return nil, false
	}
	for _, r := range records {
		if r == nil {
			return nil, false
		}
		if _, ok := r["log"]; ok {
			continue
		}
		if _, ok := r["date"]; ok {
			continue
		}
		return nil, false
	}

	out := make([]common.StandardLog, 0, len(records))
	for _, r := range records {
		var l common.StandardLog
		line, _ := r["log"].(string)
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			l = parseLine([]byte(line))
		} else {
			// no log line: keep the whole record as a JSON log
			raw, err := json.Marshal(r)
			if err != nil {
				continue
			}
			s := string(raw)
			l = common.StandardLog{Raw: s, Parsed: &s}
		}

		// inner-parser timestamp wins: FluentBit's "date" is capture time,
		// the app's own timestamp is closer to the event
		if l.Timestamp == 0 {
			if d, ok := r["date"].(float64); ok {
				l.Timestamp = toMs(d)
			}
		}
		if l.Timestamp == 0 {
			l.Timestamp = receivedAt
		}

		l.Service = service
		if cn, ok := r["container_name"].(string); ok && cn != "" {
			l.Service = strings.TrimPrefix(cn, "/")
		}
		l.Host = host
		l.IngestedAt = receivedAt
		fallbackLevel(&l)
		out = append(out, l)
	}
	return out, true
}
