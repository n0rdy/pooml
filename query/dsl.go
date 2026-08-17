package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The metrics DSL: a tiny CLOSED grammar compiled one-way to SQL (shown live
// in the editor - executable documentation). See CONTEXT.md > Future
// Enhancements > Metrics query DSL for the design decisions.
//
//	verb(metric) [key=value ...] [per DUR] [last DUR] [by service]
//
// Verbs: increase, rate, avg, min, max, sum, count, latest.
// Filter keys service/host resolve to columns; anything else to
// json_extract(labels, '$.key') - the user should not need to know where a
// dimension is stored.

type DSLFilter struct {
	Key   string
	Value string
}

type DSLQuery struct {
	Verb      string
	Metric    string
	Filters   []DSLFilter
	BucketMs  int64 // 0 = no `per`
	WindowMs  int64 // defaulted to 24h when absent
	ByService bool

	// set via OverrideWindow: an absolute window replaces the relative `last`
	fromMs, toMs int64
}

// OverrideWindow pins the query to [fromMs, toMs] regardless of its `last`
// clause - the War Room's "one time window rules the page" contract. WindowMs
// follows the span so rate()'s per-second denominator stays honest.
func (d *DSLQuery) OverrideWindow(fromMs, toMs int64) {
	d.fromMs, d.toMs = fromMs, toMs
	d.WindowMs = toMs - fromMs
}

const dslDefaultWindowMs = 24 * 3600 * 1000

// pivotServiceCap bounds `by service` pivots; more than this stops being a
// readable chart anyway.
const pivotServiceCap = 6

var dslVerbs = map[string]bool{
	"increase": true, "rate": true, "avg": true, "min": true,
	"max": true, "sum": true, "count": true, "latest": true,
}

var dslAggFn = map[string]string{
	"avg": "AVG(value)", "min": "MIN(value)", "max": "MAX(value)",
	"sum": "SUM(value)", "count": "COUNT(*)",
}

var (
	dslHeadRe = regexp.MustCompile(`^\s*([A-Za-z]+)\s*\(\s*([^)]+?)\s*\)\s*(.*)$`)
	dslDurRe  = regexp.MustCompile(`^(\d+)(m|h|d)$`)
	dslKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
)

func ParseDSL(input string) (*DSLQuery, error) {
	m := dslHeadRe.FindStringSubmatch(input)
	if m == nil {
		return nil, fmt.Errorf("start with a verb and a metric, e.g. avg(queue_depth) - verbs: increase, rate, avg, min, max, sum, count, latest")
	}
	d := &DSLQuery{Verb: strings.ToLower(m[1]), Metric: m[2]}
	if !dslVerbs[d.Verb] {
		return nil, fmt.Errorf("unknown verb %q - try: increase, rate, avg, min, max, sum, count, latest", m[1])
	}

	tokens := strings.Fields(m[3])
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch strings.ToLower(tok) {
		case "per":
			if d.BucketMs != 0 {
				return nil, fmt.Errorf("'per' given twice")
			}
			if i+1 >= len(tokens) {
				return nil, fmt.Errorf("'per' needs a duration, e.g. per 10m")
			}
			i++
			ms, err := parseDSLDuration(tokens[i])
			if err != nil {
				return nil, err
			}
			d.BucketMs = ms
		case "last":
			if d.WindowMs != 0 {
				return nil, fmt.Errorf("'last' given twice")
			}
			if i+1 >= len(tokens) {
				return nil, fmt.Errorf("'last' needs a duration, e.g. last 24h")
			}
			i++
			ms, err := parseDSLDuration(tokens[i])
			if err != nil {
				return nil, err
			}
			d.WindowMs = ms
		case "by":
			if i+1 >= len(tokens) || strings.ToLower(tokens[i+1]) != "service" {
				return nil, fmt.Errorf("only 'by service' is supported for now")
			}
			i++
			d.ByService = true
		default:
			key, val, ok := strings.Cut(tok, "=")
			if !ok {
				return nil, fmt.Errorf("didn't understand %q - expected key=value, per, last, or by service", tok)
			}
			val = strings.Trim(val, `'"`)
			if !dslKeyRe.MatchString(key) || val == "" {
				return nil, fmt.Errorf("bad filter %q - expected key=value", tok)
			}
			d.Filters = append(d.Filters, DSLFilter{Key: key, Value: val})
		}
	}

	if d.Verb == "latest" && d.BucketMs != 0 {
		return nil, fmt.Errorf("'latest' answers a point-in-time question; it doesn't take 'per'")
	}
	if d.WindowMs == 0 {
		d.WindowMs = dslDefaultWindowMs
	}
	return d, nil
}

// NeedsServices reports whether SQL generation needs the metric's service
// list (the by-service pivot hardcodes the columns - deliberately visible
// and editable in the compiled SQL).
func (d *DSLQuery) NeedsServices() bool {
	return d.ByService && d.BucketMs != 0 && d.Verb != "latest"
}

func (d *DSLQuery) SQL(services []string) (string, error) {
	if d.NeedsServices() {
		if len(services) == 0 {
			return "", fmt.Errorf("no data for metric %q yet, so 'by service' has no services to compare", d.Metric)
		}
		if len(services) > pivotServiceCap {
			services = services[:pivotServiceCap]
		}
	}
	where := d.whereClause()

	switch d.Verb {
	case "latest":
		if d.ByService {
			// SQLite: bare columns in a GROUP BY query come from the MAX row
			return fmt.Sprintf("SELECT service, latest\nFROM (\n  SELECT service, value AS latest, MAX(id) AS latest_id\n  FROM metrics\n  WHERE %s\n  GROUP BY service\n)\nORDER BY latest DESC", where), nil
		}
		return fmt.Sprintf("SELECT value AS latest\nFROM metrics\nWHERE %s\nORDER BY id DESC\nLIMIT 1", where), nil

	case "increase", "rate":
		return d.increaseSQL(where, services), nil

	default: // plain aggregates
		fn := dslAggFn[d.Verb]
		alias := d.Verb + "_value"
		if d.Verb == "count" {
			alias = "samples"
		}
		switch {
		case d.BucketMs != 0 && d.ByService:
			cols := make([]string, len(services))
			for i, s := range services {
				cols[i] = fmt.Sprintf("       %s AS %s", pivotCase(d.Verb, s), dslQuoteIdent(s))
			}
			return fmt.Sprintf("SELECT timestamp / %d * %d AS bucket,\n%s\nFROM metrics\nWHERE %s\nGROUP BY bucket\nORDER BY bucket", d.BucketMs, d.BucketMs, strings.Join(cols, ",\n"), where), nil
		case d.BucketMs != 0:
			return fmt.Sprintf("SELECT timestamp / %d * %d AS bucket, %s AS %s\nFROM metrics\nWHERE %s\nGROUP BY bucket\nORDER BY bucket", d.BucketMs, d.BucketMs, fn, alias, where), nil
		case d.ByService:
			return fmt.Sprintf("SELECT service, %s AS %s\nFROM metrics\nWHERE %s\nGROUP BY service\nORDER BY %s DESC", fn, alias, where, alias), nil
		default:
			// single number over the window: a stat
			return fmt.Sprintf("SELECT %s AS %s\nFROM metrics\nWHERE %s", fn, alias, where), nil
		}
	}
}

// increaseSQL: a counter's increase is computed per series (service, host,
// labels) first - MAX-MIN within the frame - then summed. rate divides by
// the frame length in seconds.
func (d *DSLQuery) increaseSQL(where string, services []string) string {
	frameSecs := d.WindowMs / 1000
	inner := fmt.Sprintf("  SELECT service, host, labels, MAX(value) - MIN(value) AS inc\n  FROM metrics\n  WHERE %s\n  GROUP BY service, host, labels", where)
	outerVal := "SUM(inc)"
	alias := "increase"
	if d.Verb == "rate" {
		alias = "rate_per_s"
	}

	if d.BucketMs != 0 {
		frameSecs = d.BucketMs / 1000
		inner = fmt.Sprintf("  SELECT timestamp / %d * %d AS bucket, service, host, labels, MAX(value) - MIN(value) AS inc\n  FROM metrics\n  WHERE %s\n  GROUP BY bucket, service, host, labels", d.BucketMs, d.BucketMs, where)
	}
	if d.Verb == "rate" {
		outerVal = fmt.Sprintf("SUM(inc) / %d.0", frameSecs)
	}

	switch {
	case d.BucketMs != 0 && d.ByService:
		cols := make([]string, len(services))
		for i, s := range services {
			val := fmt.Sprintf("SUM(CASE WHEN service = '%s' THEN inc END)", dslQuoteStr(s))
			if d.Verb == "rate" {
				val = fmt.Sprintf("%s / %d.0", val, frameSecs)
			}
			cols[i] = fmt.Sprintf("       %s AS %s", val, dslQuoteIdent(s))
		}
		return fmt.Sprintf("SELECT bucket,\n%s\nFROM (\n%s\n)\nGROUP BY bucket\nORDER BY bucket", strings.Join(cols, ",\n"), inner)
	case d.BucketMs != 0:
		return fmt.Sprintf("SELECT bucket, %s AS %s\nFROM (\n%s\n)\nGROUP BY bucket\nORDER BY bucket", outerVal, alias, inner)
	case d.ByService:
		return fmt.Sprintf("SELECT service, %s AS %s\nFROM (\n%s\n)\nGROUP BY service\nORDER BY %s DESC", outerVal, alias, inner, alias)
	default:
		return fmt.Sprintf("SELECT %s AS %s\nFROM (\n%s\n)", outerVal, alias, inner)
	}
}

func (d *DSLQuery) whereClause() string {
	conds := []string{fmt.Sprintf("name = '%s'", dslQuoteStr(d.Metric))}
	for _, f := range d.Filters {
		switch f.Key {
		case "service", "host":
			conds = append(conds, fmt.Sprintf("%s = '%s'", f.Key, dslQuoteStr(f.Value)))
		default:
			conds = append(conds, fmt.Sprintf("json_extract(labels, '$.%s') = '%s'", dslQuoteStr(f.Key), dslQuoteStr(f.Value)))
		}
	}
	if d.toMs > 0 {
		conds = append(conds, fmt.Sprintf("timestamp BETWEEN %d AND %d", d.fromMs, d.toMs))
	} else {
		conds = append(conds, fmt.Sprintf("timestamp > (unixepoch() - %d) * 1000", d.WindowMs/1000))
	}
	return strings.Join(conds, " AND ")
}

func pivotCase(verb, service string) string {
	if verb == "count" {
		return fmt.Sprintf("COUNT(CASE WHEN service = '%s' THEN 1 END)", dslQuoteStr(service))
	}
	fn := strings.ToUpper(verb)
	return fmt.Sprintf("%s(CASE WHEN service = '%s' THEN value END)", fn, dslQuoteStr(service))
}

func parseDSLDuration(s string) (int64, error) {
	m := dslDurRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("bad duration %q - use forms like 5m, 1h, 2d", s)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	if n == 0 {
		return 0, fmt.Errorf("duration can't be zero")
	}
	switch m[2] {
	case "m":
		return n * 60_000, nil
	case "h":
		return n * 3_600_000, nil
	default:
		return n * 86_400_000, nil
	}
}

func dslQuoteStr(s string) string   { return strings.ReplaceAll(s, "'", "''") }
func dslQuoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
