package templates

import (
	"fmt"
	"net/url"
	"strings"
)

type VolumePoint struct {
	BucketMs int64
	Count    int64
}

type HomeVolumeView struct {
	Points []VolumePoint
	Max    int64
	Total  int64
}

type HomeErrorRow struct {
	Service string
	Count   int64
}

type HomeServiceRow struct {
	Service    string
	LastSeenMs int64
	Count      int64
}

// TriageQuery is the one-click incident view: errors and fatals, last hour.
const TriageQuery = "SELECT * FROM logs WHERE level >= 4 AND timestamp > unixepoch() * 1000 - 3600000 ORDER BY timestamp DESC LIMIT 100"

func triageURL() string {
	return "/logs?" + url.Values{"q": {TriageQuery}, "live": {"1"}}.Encode()
}

func sqlString(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

func serviceLogsURL(service string) string {
	q := "SELECT * FROM logs WHERE service = " + sqlString(service) + " ORDER BY timestamp DESC LIMIT 100"
	return "/logs?" + url.Values{"q": {q}}.Encode()
}

func serviceErrorsURL(service string) string {
	q := "SELECT * FROM logs WHERE service = " + sqlString(service) +
		" AND level >= 4 AND timestamp > unixepoch() * 1000 - 86400000 ORDER BY timestamp DESC LIMIT 100"
	return "/logs?" + url.Values{"q": {q}}.Encode()
}

// barHeight scales a bucket against the busiest one; a non-empty bucket never
// renders shorter than 4% so activity stays visible.
func barHeight(count, max int64) string {
	if count == 0 || max == 0 {
		return "height: 2px"
	}
	pct := float64(count) / float64(max) * 100
	if pct < 4 {
		pct = 4
	}
	return fmt.Sprintf("height: %.0f%%", pct)
}

func barTitle(p VolumePoint) string {
	return fmt.Sprintf("%s - %d logs", utcTime(p.BucketMs), p.Count)
}
