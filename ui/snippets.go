package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/n0rdy/pooml/ui/templates"
)

// Snippets teach the hard query patterns. Deliberately generic with
// screaming placeholders, never prefilled from the catalog: picking a name
// for the user means interpreting names, and that guessing game reads as
// magic when right and as a bug when wrong. The catalogs (explorer landing,
// logs quick filters) show what exists; the snippets show what to do with it.
const snippetPlaceholderMarker = "REPLACE_WITH_"

var explorerSnippets = []templates.QuerySnippet{
	{Label: "counter increase / hour",
		DSL: "increase(REPLACE_WITH_YOUR_COUNTER_NAME) per 1h last 24h",
		SQL: "SELECT hour, SUM(inc) AS increase\nFROM (\n  SELECT timestamp / 3600000 * 3600000 AS hour, labels,\n         MAX(value) - MIN(value) AS inc\n  FROM metrics\n  WHERE name = 'REPLACE_WITH_YOUR_COUNTER_NAME' AND timestamp > (unixepoch() - 86400) * 1000\n  GROUP BY hour, labels\n)\nGROUP BY hour\nORDER BY hour"},
	{Label: "gauge avg over time",
		DSL: "avg(REPLACE_WITH_YOUR_GAUGE_NAME) per 10m last 24h",
		SQL: "SELECT timestamp / 600000 * 600000 AS bucket, AVG(value) AS avg_value\nFROM metrics\nWHERE name = 'REPLACE_WITH_YOUR_GAUGE_NAME' AND timestamp > (unixepoch() - 86400) * 1000\nGROUP BY bucket\nORDER BY bucket"},
	{Label: "avg from _sum/_count", SQL: "SELECT s.timestamp, s.value / c.value AS avg_value\nFROM metrics s\nJOIN metrics c ON c.timestamp = s.timestamp AND c.service = s.service AND c.name = 'REPLACE_WITH_BASE_NAME_count'\nWHERE s.name = 'REPLACE_WITH_BASE_NAME_sum'\nORDER BY s.timestamp"},
	{Label: "pivot: compare services", SQL: "SELECT timestamp / 600000 * 600000 AS bucket,\n       AVG(CASE WHEN service = 'REPLACE_WITH_SERVICE_A' THEN value END) AS service_a,\n       AVG(CASE WHEN service = 'REPLACE_WITH_SERVICE_B' THEN value END) AS service_b\nFROM metrics\nWHERE name = 'REPLACE_WITH_YOUR_GAUGE_NAME'\nGROUP BY bucket\nORDER BY bucket"},
	{Label: "top metrics", SQL: "SELECT name, COUNT(*) AS datapoints\nFROM metrics\nGROUP BY name\nORDER BY datapoints DESC\nLIMIT 20"},
	{Label: "latest per service",
		DSL: "latest(REPLACE_WITH_YOUR_GAUGE_NAME) by service",
		SQL: "SELECT service, value AS latest\nFROM metrics m\nWHERE name = 'REPLACE_WITH_YOUR_GAUGE_NAME'\n  AND id = (SELECT MAX(id) FROM metrics WHERE name = m.name AND service = m.service)"},
}

// Logs snippets cover the aggregation tier only: the simple tier is already
// served by the FTS bar and quick-filter badges. host earns a snippet
// because it lost its click path when the column left the viewer.
var logsSnippets = []templates.QuerySnippet{
	{Label: "error count per 10 min (24h)", SQL: "SELECT timestamp / 600000 * 600000 AS bucket, COUNT(*) AS errors\nFROM logs\nWHERE level >= 4 AND timestamp > (unixepoch() - 86400) * 1000\nGROUP BY bucket\nORDER BY bucket"},
	{Label: "errors vs total per service (24h)", SQL: "SELECT service,\n       SUM(CASE WHEN level >= 4 THEN 1 ELSE 0 END) AS errors,\n       COUNT(*) AS total\nFROM logs\nWHERE timestamp > (unixepoch() - 86400) * 1000\nGROUP BY service\nORDER BY errors DESC"},
	{Label: "extract a field from parsed JSON", SQL: "SELECT timestamp, json_extract(parsed, '$.REPLACE_WITH_FIELD_NAME') AS field, message\nFROM logs\nWHERE parsed IS NOT NULL\nORDER BY timestamp DESC\nLIMIT 100"},
	{Label: "logs from one host + service", SQL: "SELECT *\nFROM logs\nWHERE host = 'REPLACE_WITH_HOST_NAME'\n  AND service = 'REPLACE_WITH_SERVICE_NAME'\nORDER BY timestamp DESC\nLIMIT 100"},
	{Label: "logs at one level", SQL: "SELECT *\nFROM logs\n-- levels: 0=trace  1=debug  2=info  3=warn  4=error  5=fatal\n-- (level >= 4 catches errors AND fatals)\nWHERE level = 4\nORDER BY timestamp DESC\nLIMIT 100"},
}

var placeholderRe = regexp.MustCompile(`REPLACE_WITH_[A-Z_]+`)

// placeholderErr names the exact placeholder and what belongs in its place.
func placeholderErr(q string) string {
	ph := placeholderRe.FindString(q)
	what := "value"
	switch {
	case strings.Contains(ph, "SERVICE"):
		what = "service name"
	case strings.Contains(ph, "HOST"):
		what = "host name"
	case strings.Contains(ph, "FIELD"):
		what = "field name"
	case strings.Contains(ph, "COUNTER"), strings.Contains(ph, "GAUGE"), strings.Contains(ph, "BASE"), strings.Contains(ph, "METRIC"):
		what = "metric name"
	}
	return fmt.Sprintf("Replace %s with a real %s, then run again.", ph, what)
}
