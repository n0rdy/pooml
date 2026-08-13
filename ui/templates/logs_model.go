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
	IsViewer     bool
	Rows         []LogRow // ascending by id: newest at the bottom
	HasMore      bool
	AtStart      bool // pagination probe found nothing older
	NextBeforeTs int64
	NextBeforeID int64

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
// one visual family (badge-soft: tinted bg + border) so levels differ only
// by color, per field feedback that mixed styles read as inconsistency
var levelClasses = map[int]string{
	0: "badge-soft",
	1: "badge-soft",
	2: "badge-soft badge-info",
	3: "badge-soft badge-warning",
	4: "badge-soft badge-error",
	5: "badge-soft badge-error font-bold",
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
	base := "badge badge-sm font-mono cursor-pointer"
	if l == nil {
		return base + " badge-soft opacity-40"
	}
	if c, ok := levelClasses[*l]; ok {
		return base + " " + c
	}
	return base + " badge-soft"
}

func filterURL(q, col, val string) string {
	return "/logs?" + url.Values{"q": {q}, col: {val}}.Encode()
}

func pageURL(v LogsView) string {
	vals := url.Values{
		"q":         {v.Q},
		"before_ts": {strconv.FormatInt(v.NextBeforeTs, 10)},
		"before_id": {strconv.FormatInt(v.NextBeforeID, 10)},
	}
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

// ~14 chars is where the badge's max-w-28 starts ellipsizing; shorter names
// get no tooltip because there is nothing to reveal. Hover text is identity
// only - never action hints (those are affordance + feedback's job).
func serviceBadgeClass(name string) string {
	base := "badge badge-sm badge-outline font-mono cursor-pointer max-w-28 min-w-0"
	if len(name) > 14 {
		return base + " tooltip"
	}
	return base
}

func serviceTip(name string) string {
	if len(name) > 14 {
		return name
	}
	return ""
}

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
