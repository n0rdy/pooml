package ingestion

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/n0rdy/pooml/common"
)

const receivedAt = int64(1_755_000_000_000)

func intPtr(v int) *int { return &v }

func TestParsePayloadSingleLines(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantLevel *int
		wantTs    int64 // 0 = expect receivedAt fallback
		wantMsg   string
		wantInPd  []string // substrings expected in Parsed; nil = Parsed must be nil
	}{
		{
			name:      "zerolog json",
			line:      `{"level":"error","time":"2026-08-12T10:00:00Z","message":"boom"}`,
			wantLevel: intPtr(common.LevelError),
			wantTs:    1_786_528_800_000,
			wantMsg:   "boom",
			wantInPd:  []string{`"level":"error"`},
		},
		{
			name:      "json epoch seconds ts and msg key",
			line:      `{"level":"info","ts":1754990000.5,"msg":"hi"}`,
			wantLevel: intPtr(common.LevelInfo),
			wantTs:    1_754_990_000_500,
			wantMsg:   "hi",
			wantInPd:  []string{`"ts":1754990000.5`},
		},
		{
			name:      "json epoch millis ts",
			line:      `{"level":"warn","timestamp":1754990000123,"msg":"careful"}`,
			wantLevel: intPtr(common.LevelWarn),
			wantTs:    1_754_990_000_123,
			wantMsg:   "careful",
			wantInPd:  []string{`"timestamp"`},
		},
		{
			name:      "json bunyan numeric level",
			line:      `{"level":30,"msg":"bunyan style"}`,
			wantLevel: intPtr(common.LevelInfo),
			wantMsg:   "bunyan style",
			wantInPd:  []string{`"level":30`},
		},
		{
			name:     "json unknown level stays nil",
			line:     `{"level":"verbose","msg":"odd"}`,
			wantMsg:  "odd",
			wantInPd: []string{`"level":"verbose"`},
		},
		{
			name:      "clf 200 is info",
			line:      `192.168.1.10 - alice [12/Aug/2026:10:15:30 +0000] "GET /api/v1/users HTTP/1.1" 200 512 "https://ref.example" "curl/8.5.0"`,
			wantLevel: intPtr(common.LevelInfo),
			wantTs:    1_786_529_730_000,
			wantMsg:   "GET /api/v1/users 200",
			wantInPd:  []string{`"ip":"192.168.1.10"`, `"status":200`, `"user":"alice"`, `"referer":"https://ref.example"`, `"user_agent":"curl/8.5.0"`},
		},
		{
			name:      "clf 404 is warn",
			line:      `10.0.0.1 - - [12/Aug/2026:10:15:30 +0000] "GET /missing HTTP/1.1" 404 - "-" "-"`,
			wantLevel: intPtr(common.LevelWarn),
			wantTs:    1_786_529_730_000,
			wantMsg:   "GET /missing 404",
			wantInPd:  []string{`"status":404`},
		},
		{
			name:      "clf 500 is error, no combined tail",
			line:      `10.0.0.1 - - [12/Aug/2026:10:15:30 +0000] "POST /pay HTTP/2.0" 500 123`,
			wantLevel: intPtr(common.LevelError),
			wantTs:    1_786_529_730_000,
			wantMsg:   "POST /pay 500",
			wantInPd:  []string{`"status":500`, `"bytes":123`},
		},
		{
			name:      "plain with rfc3339 ts and level",
			line:      `2026-08-12T10:00:00Z ERROR something failed`,
			wantLevel: intPtr(common.LevelError),
			wantTs:    1_786_528_800_000,
			wantMsg:   "2026-08-12T10:00:00Z ERROR something failed",
		},
		{
			name:      "plain with space-separated ts",
			line:      `2026-08-12 10:00:00 WARN watch out`,
			wantLevel: intPtr(common.LevelWarn),
			wantTs:    1_786_528_800_000,
			wantMsg:   "2026-08-12 10:00:00 WARN watch out",
		},
		{
			name:    "plain no ts no level",
			line:    `hello world`,
			wantMsg: "hello world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := parsePayload("svc", "hst", []byte(tt.line), receivedAt)
			if len(logs) != 1 {
				t.Fatalf("got %d logs, want 1", len(logs))
			}
			l := logs[0]

			if l.Service != "svc" || l.Host != "hst" {
				t.Errorf("service/host = %q/%q", l.Service, l.Host)
			}
			if l.IngestedAt != receivedAt {
				t.Errorf("ingestedAt = %d", l.IngestedAt)
			}
			if l.Raw != tt.line {
				t.Errorf("raw = %q, want original line", l.Raw)
			}

			wantTs := tt.wantTs
			if wantTs == 0 {
				wantTs = receivedAt
			}
			if l.Timestamp != wantTs {
				t.Errorf("timestamp = %d, want %d", l.Timestamp, wantTs)
			}

			switch {
			case tt.wantLevel == nil && l.Level != nil:
				t.Errorf("level = %d, want nil", *l.Level)
			case tt.wantLevel != nil && l.Level == nil:
				t.Errorf("level = nil, want %d", *tt.wantLevel)
			case tt.wantLevel != nil && *l.Level != *tt.wantLevel:
				t.Errorf("level = %d, want %d", *l.Level, *tt.wantLevel)
			}

			if tt.wantMsg != "" {
				if l.Message == nil || *l.Message != tt.wantMsg {
					t.Errorf("message = %v, want %q", l.Message, tt.wantMsg)
				}
			}

			if tt.wantInPd == nil {
				if l.Parsed != nil {
					t.Errorf("parsed = %q, want nil", *l.Parsed)
				}
			} else {
				if l.Parsed == nil {
					t.Fatalf("parsed = nil, want substrings %v", tt.wantInPd)
				}
				for _, sub := range tt.wantInPd {
					if !strings.Contains(*l.Parsed, sub) {
						t.Errorf("parsed %q missing %q", *l.Parsed, sub)
					}
				}
				if !json.Valid([]byte(*l.Parsed)) {
					t.Errorf("parsed is not valid JSON: %q", *l.Parsed)
				}
			}
		})
	}
}

func TestParsePayloadMultiline(t *testing.T) {
	payload := "{\"level\":\"info\",\"msg\":\"one\"}\r\n\n  \nplain two\n"
	logs := parsePayload("svc", "hst", []byte(payload), receivedAt)
	if len(logs) != 2 {
		t.Fatalf("got %d logs, want 2 (empty lines skipped)", len(logs))
	}
	if logs[0].Message == nil || *logs[0].Message != "one" {
		t.Errorf("first message = %v", logs[0].Message)
	}
	if logs[1].Raw != "plain two" {
		t.Errorf("second raw = %q (CR not trimmed?)", logs[1].Raw)
	}
}

func TestParsePayloadEmpty(t *testing.T) {
	if logs := parsePayload("svc", "hst", []byte("  \n \n"), receivedAt); len(logs) != 0 {
		t.Fatalf("got %d logs from blank payload, want 0", len(logs))
	}
}

func TestParseFluentBitEnvelope(t *testing.T) {
	payload := `[
		{"date":1754990000.0,"log":"{\"level\":\"warn\",\"msg\":\"inner\"}\n","container_name":"/payment-svc"},
		{"date":1754990001.0,"log":"plain inner line\n"},
		{"date":1754990002.0,"container_name":"/no-log-svc","source":"stderr"}
	]`
	logs := parsePayload("path-svc", "path-host", []byte(payload), receivedAt)
	if len(logs) != 3 {
		t.Fatalf("got %d logs, want 3", len(logs))
	}

	// record 1: JSON inner line, container_name overrides path service
	if logs[0].Service != "payment-svc" {
		t.Errorf("service = %q, want payment-svc (container_name, slash stripped)", logs[0].Service)
	}
	if logs[0].Level == nil || *logs[0].Level != common.LevelWarn {
		t.Errorf("level = %v, want warn (from inner JSON)", logs[0].Level)
	}
	if logs[0].Timestamp != 1_754_990_000_000 {
		t.Errorf("timestamp = %d, want envelope date (inner has none)", logs[0].Timestamp)
	}
	if logs[0].Raw != `{"level":"warn","msg":"inner"}` {
		t.Errorf("raw = %q, want unwrapped inner line without trailing newline", logs[0].Raw)
	}

	// record 2: no container_name, falls back to path service
	if logs[1].Service != "path-svc" {
		t.Errorf("service = %q, want path-svc", logs[1].Service)
	}
	if logs[1].Timestamp != 1_754_990_001_000 {
		t.Errorf("timestamp = %d", logs[1].Timestamp)
	}

	// record 3: no log line, whole record kept as JSON
	if logs[2].Parsed == nil || !strings.Contains(*logs[2].Parsed, `"source":"stderr"`) {
		t.Errorf("parsed = %v, want full record JSON", logs[2].Parsed)
	}
	if logs[2].Service != "no-log-svc" {
		t.Errorf("service = %q", logs[2].Service)
	}
}

func TestParseFluentBitInnerTimestampWins(t *testing.T) {
	payload := `[{"date":1754990000.0,"log":"{\"level\":\"info\",\"ts\":1754985000,\"msg\":\"own ts\"}"}]`
	logs := parsePayload("svc", "hst", []byte(payload), receivedAt)
	if len(logs) != 1 {
		t.Fatalf("got %d logs", len(logs))
	}
	if logs[0].Timestamp != 1_754_985_000_000 {
		t.Errorf("timestamp = %d, want inner app ts, not envelope date", logs[0].Timestamp)
	}
}

func TestNonEnvelopeJSONArrayFallsThrough(t *testing.T) {
	payload := `[1, 2, 3]`
	logs := parsePayload("svc", "hst", []byte(payload), receivedAt)
	if len(logs) != 1 {
		t.Fatalf("got %d logs, want 1 (single plain line)", len(logs))
	}
	if logs[0].Raw != payload {
		t.Errorf("raw = %q", logs[0].Raw)
	}
}

func TestBroadcasterRingAndSubscribers(t *testing.T) {
	b := NewBroadcaster()
	ch, unsub := b.Subscribe()

	for i := range 1500 {
		msg := string(rune('a' + i%26))
		b.broadcast(common.StandardLog{Raw: msg, Timestamp: int64(i)})
	}

	back := b.Backfill()
	if len(back) != ringSize {
		t.Fatalf("backfill len = %d, want %d", len(back), ringSize)
	}
	if back[0].Timestamp != 500 || back[ringSize-1].Timestamp != 1499 {
		t.Errorf("ring window = [%d, %d], want [500, 1499]", back[0].Timestamp, back[ringSize-1].Timestamp)
	}

	// subscriber channel holds only subscriberBufSize; the rest were dropped,
	// never blocking the broadcast side
	if got := len(ch); got != subscriberBufSize {
		t.Errorf("subscriber buffered %d, want %d", got, subscriberBufSize)
	}

	unsub()
	if _, ok := <-drainClosed(ch); ok {
		t.Error("channel should be closed after unsubscribe")
	}
}

func drainClosed(ch <-chan common.StandardLog) <-chan common.StandardLog {
	for range len(ch) {
		<-ch
	}
	return ch
}
