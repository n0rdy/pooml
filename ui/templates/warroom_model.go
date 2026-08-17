package templates

import (
	"fmt"

	"github.com/a-h/templ"
)

// WarDefaultLogsQuery: warnings and up - the War Room is an incident surface,
// INFO noise stays out until asked for.
const WarDefaultLogsQuery = "SELECT * FROM logs WHERE level >= 3 ORDER BY timestamp DESC LIMIT 500"

// WarRoomView is the whole page state; everything here round-trips through
// the URL (M12 decision: ad-hoc only, no persistence).
type WarRoomView struct {
	FromMs int64
	ToMs   int64
	Follow bool

	Metrics []WarMetricRow // always 3 rows; empty DSL = empty row

	MiniChart  *ChartPayload // nil = volume query failed
	MiniTotal  int64
	MiniErrors int64

	Logs LogsView

	MetricNames []string
}

type WarMetricRow struct {
	Index  int
	DSL    string
	Err    string
	Shaped ShapedResult // zero-value Kind "" when the row is empty
}

func oobAttr(oob bool) templ.Attributes {
	if !oob {
		return templ.Attributes{}
	}
	return templ.Attributes{"hx-swap-oob": "true"}
}

func warChartID(i int) string { return fmt.Sprintf("war-chart-%d", i) }

func warMiniLabel(v WarRoomView) string {
	return fmt.Sprintf("%d logs · %d errors in window", v.MiniTotal, v.MiniErrors)
}
