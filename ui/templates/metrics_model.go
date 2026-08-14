package templates

import "net/url"

func metricsExportURL(q, format string) string {
	vals := url.Values{"q": {q}, "format": {format}}
	return "/metrics-explorer/export?" + vals.Encode()
}

func metricsViewURL(q, view string) string {
	vals := url.Values{"q": {q}, "view": {view}}
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
