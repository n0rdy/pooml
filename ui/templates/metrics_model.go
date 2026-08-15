package templates

import "net/url"

func metricsExportURL(q, format string) string {
	vals := url.Values{"q": {q}, "format": {format}}
	return "/metrics-explorer/export?" + vals.Encode()
}

func metricsViewURL(q, dsl, view string) string {
	vals := url.Values{"q": {q}, "view": {view}}
	if dsl != "" {
		vals.Set("dsl", dsl)
	}
	return "/metrics-explorer?" + vals.Encode()
}

// autoViewLabel shows auto's verdict ("auto (line)") so the active view is
// never ambiguous.
func autoViewLabel(v MetricsExplorerView) string {
	if v.View != "auto" {
		return "auto"
	}
	switch v.Shaped.Kind {
	case "chart":
		return "auto (" + v.Shaped.Chart.Type + ")"
	case "stat", "table":
		return "auto (" + v.Shaped.Kind + ")"
	}
	return "auto"
}
