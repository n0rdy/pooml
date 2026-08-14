package templates

import "net/url"

func metricsExportURL(q, format string) string {
	vals := url.Values{"q": {q}, "format": {format}}
	return "/metrics-explorer/export?" + vals.Encode()
}
