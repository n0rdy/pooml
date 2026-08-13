package templates

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

type LogsView struct {
	Q   string
	FTS string
	Op  string

	Error string

	// log-viewer shape
	IsViewer   bool
	Rows       []LogRow // ascending by id: newest at the bottom
	HasMore    bool
	AtStart    bool // pagination probe found nothing older
	NextBefore int64

	// generic table shape
	Columns []string
	Cells   [][]string

	Truncated bool
}

type LogRow struct {
	ID      int64
	Ts      int64
	Level   *int
	Service string
	Host    string
	Message string
	Raw     string
}

type LogDetail struct {
	ID         int64
	Ts         int64
	IngestedAt int64
	Level      *int
	Service    string
	Host       string
	Raw        string
	Parsed     string
}

var levelNames = map[int]string{0: "TRACE", 1: "DEBUG", 2: "INFO", 3: "WARN", 4: "ERROR", 5: "FATAL"}

// badge classes lean on the theme's semantic colors: the ANSI-ish register of
// the log viewer, per CONTEXT.md > UI Design Direction
var levelClasses = map[int]string{
	0: "badge-ghost",
	1: "badge-ghost",
	2: "badge-info badge-outline",
	3: "badge-warning",
	4: "badge-error",
	5: "badge-error font-bold",
}

func levelName(l *int) string {
	if l == nil {
		return "-"
	}
	if n, ok := levelNames[*l]; ok {
		return n
	}
	return strconv.Itoa(*l)
}

func levelClass(l *int) string {
	base := "badge badge-xs font-mono cursor-pointer"
	if l == nil {
		return base + " badge-ghost opacity-40"
	}
	if c, ok := levelClasses[*l]; ok {
		return base + " " + c
	}
	return base + " badge-ghost"
}

func filterURL(q, col, val string) string {
	return "/logs?" + url.Values{"q": {q}, col: {val}}.Encode()
}

func pageURL(v LogsView, before int64) string {
	vals := url.Values{"q": {v.Q}, "before": {strconv.FormatInt(before, 10)}}
	if v.FTS != "" {
		vals.Set("fts", v.FTS)
		vals.Set("op", v.Op)
	}
	return "/logs?" + vals.Encode()
}

func exportURL(v LogsView, format string) string {
	vals := url.Values{"q": {v.Q}, "format": {format}}
	if v.FTS != "" {
		vals.Set("fts", v.FTS)
		vals.Set("op", v.Op)
	}
	return "/logs/export?" + vals.Encode()
}

// utcTime is the no-JS fallback; the page script rewrites [data-ts] to the
// browser's local zone
func utcTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05.000")
}

func msString(ms int64) string { return strconv.FormatInt(ms, 10) }

func rowID(id int64) string { return fmt.Sprintf("%d", id) }

func levelValue(l *int) string {
	if l == nil {
		return ""
	}
	return strconv.Itoa(*l)
}

func rowsLabel(n int, truncated bool) string {
	label := fmt.Sprintf("%d rows", n)
	if n == 1 {
		label = "1 row"
	}
	if truncated {
		label += " (capped at 10000; narrow the query for the rest)"
	}
	return label
}
