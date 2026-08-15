// Package query is the validation, rewrite, and execution engine for
// user-supplied SQL. See CONTEXT.md > Querying.
package query

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	sqlp "github.com/rqlite/sql"
)

// MaxRows caps every result: injected as LIMIT when absent, clamped when
// larger, and enforced again at scan time in Execute for non-literal limits.
const MaxRows = 10000

type Shape int

const (
	ShapeLogViewer Shape = iota
	ShapeAggregation
)

var (
	ErrNotSelect          = errors.New("only SELECT queries are allowed")
	ErrMultipleStatements = errors.New("only a single SQL statement is allowed")
	ErrUnsupportedShape   = errors.New("not supported for this query shape")
)

// metrics lives in metrics.db, attached read-only to every logs-read
// connection (db/pool.go), so unqualified `metrics` resolves there. Which
// tables a given surface may query is a Scope decision, not engine-global:
// signals are deliberately isolated in the UI (see CONTEXT.md > Querying).
var allowedTables = map[string]bool{"logs": true, "logs_fts": true, "metrics": true}

// Scope is the per-surface allow-list. Free-form cross-signal JOINs are a
// foot-gun (no natural join key between logs and metrics), so only alerts
// keep ScopeAll: their output is fires-or-not, never rendered. Every
// rendered surface (logs page, explorer, dashboard panels via their typed
// dashboard) gets exactly one signal.
type Scope int

const (
	ScopeAll Scope = iota
	ScopeLogs
	ScopeMetrics
)

var bannedFunctions = map[string]bool{"load_extension": true}

var aggregateFunctions = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true,
	"total": true, "group_concat": true, "string_agg": true,
}

// Validated is a parsed and vetted SELECT. Rewrites mutate the AST in place;
// SQL() serializes the current state.
type Validated struct {
	stmt     *sqlp.SelectStatement
	Shape    Shape
	Compound bool // UNION/INTERSECT/EXCEPT: executable, but rewrites refuse it
}

func (v *Validated) SQL() string { return prettify(v.stmt.String()) }

// Validate is the ScopeAll form, used by alerts.
func Validate(q string) (*Validated, error) {
	return ValidateIn(q, ScopeAll)
}

func ValidateIn(q string, scope Scope) (*Validated, error) {
	stmts, err := sqlp.NewParser(strings.NewReader(q)).ParseStatements()
	if err != nil {
		// the parser treats bare rowid as a keyword; SQLite-style
		// `logs_fts.rowid` needs double quotes here
		if strings.Contains(err.Error(), "'ROWID'") {
			return nil, fmt.Errorf(`invalid SQL: %v (hint: write "rowid" in double quotes)`, err)
		}
		return nil, fmt.Errorf("invalid SQL: %w", err)
	}
	if len(stmts) == 0 {
		return nil, ErrNotSelect
	}
	if len(stmts) > 1 {
		return nil, ErrMultipleStatements
	}
	sel, ok := stmts[0].(*sqlp.SelectStatement)
	if !ok {
		return nil, ErrNotSelect
	}

	// CTE names first: they are legal FROM targets in the second pass
	cteNames, err := collectCTEs(sel)
	if err != nil {
		return nil, err
	}
	cv := &checkVisitor{cteNames: cteNames, referenced: map[string]bool{}}
	if _, err := sqlp.Walk(cv, sel); err != nil {
		return nil, err
	}
	if err := checkScope(scope, cv.referenced); err != nil {
		return nil, err
	}

	v := &Validated{stmt: sel, Shape: detectShape(sel), Compound: sel.Compound != nil}
	clampLimit(sel)
	return v, nil
}

func checkScope(scope Scope, referenced map[string]bool) error {
	logsUsed := referenced["logs"] || referenced["logs_fts"]
	metricsUsed := referenced["metrics"]
	switch scope {
	case ScopeLogs:
		if metricsUsed {
			return errors.New("the metrics table is not available here: query it on the Metrics page")
		}
	case ScopeMetrics:
		if logsUsed {
			return errors.New("the logs tables are not available here: query them on the Logs page")
		}
	}
	return nil
}

// The library's Walk does NOT descend into CTE bodies (WithClause) or
// subqueries in expressions (SelectExpr, e.g. `IN (SELECT ...)`) - neither has
// a case in its walk switch. Both validation passes recurse manually; without
// this, `WITH x AS (SELECT * FROM sqlite_master) ...` and
// `WHERE id IN (SELECT ... FROM api_keys)` would pass validation.
func collectCTEs(sel *sqlp.SelectStatement) (map[string]bool, error) {
	c := &cteCollector{names: make(map[string]bool)}
	_, err := sqlp.Walk(c, sel)
	return c.names, err
}

type cteCollector struct {
	names map[string]bool
}

func (c *cteCollector) Visit(n sqlp.Node) (sqlp.Visitor, sqlp.Node, error) {
	switch t := n.(type) {
	case *sqlp.WithClause:
		if t.Recursive.IsValid() {
			return nil, nil, errors.New("WITH RECURSIVE is not allowed")
		}
		for _, cte := range t.CTEs {
			c.names[strings.ToLower(cte.TableName.Name)] = true
			if _, err := sqlp.Walk(c, cte.Select); err != nil {
				return nil, nil, err
			}
		}
	case sqlp.SelectExpr:
		if _, err := sqlp.Walk(c, t.SelectStatement); err != nil {
			return nil, nil, err
		}
	}
	return c, n, nil
}

func (c *cteCollector) VisitEnd(n sqlp.Node) (sqlp.Node, error) { return n, nil }

type checkVisitor struct {
	cteNames   map[string]bool
	referenced map[string]bool // real tables seen; feeds the Scope check
}

func (c *checkVisitor) Visit(n sqlp.Node) (sqlp.Visitor, sqlp.Node, error) {
	switch t := n.(type) {
	case *sqlp.WithClause:
		for _, cte := range t.CTEs {
			if _, err := sqlp.Walk(c, cte.Select); err != nil {
				return nil, nil, err
			}
		}
	case sqlp.SelectExpr:
		if _, err := sqlp.Walk(c, t.SelectStatement); err != nil {
			return nil, nil, err
		}
	case *sqlp.QualifiedTableName:
		if t.Schema != nil {
			return nil, nil, fmt.Errorf("schema-qualified table %q is not allowed", t.Schema.Name+"."+t.Name.Name)
		}
		name := strings.ToLower(t.Name.Name)
		if !allowedTables[name] && !c.cteNames[name] {
			return nil, nil, fmt.Errorf("table %q is not allowed: only logs, logs_fts and metrics can be queried", t.Name.Name)
		}
		if allowedTables[name] && !c.cteNames[name] {
			c.referenced[name] = true
		}
	case *sqlp.Call:
		if bannedFunctions[strings.ToLower(t.Name.Name)] {
			return nil, nil, fmt.Errorf("function %q is not allowed", t.Name.Name)
		}
	}
	return c, n, nil
}

func (c *checkVisitor) VisitEnd(n sqlp.Node) (sqlp.Node, error) { return n, nil }

// visitFunc adapts a plain callback to the Visitor interface.
type visitFunc func(sqlp.Node) error

func (f visitFunc) Visit(n sqlp.Node) (sqlp.Visitor, sqlp.Node, error) {
	if err := f(n); err != nil {
		return nil, nil, err
	}
	return f, n, nil
}

func (f visitFunc) VisitEnd(n sqlp.Node) (sqlp.Node, error) { return n, nil }

func detectShape(s *sqlp.SelectStatement) Shape {
	if len(s.GroupByExprs) > 0 || s.HavingExpr != nil || s.Distinct.IsValid() || len(s.Windows) > 0 {
		return ShapeAggregation
	}
	for _, col := range s.Columns {
		if col.Expr != nil && exprHasAggregate(col.Expr) {
			return ShapeAggregation
		}
	}
	if s.Compound != nil && detectShape(s.Compound) == ShapeAggregation {
		return ShapeAggregation
	}
	return ShapeLogViewer
}

func exprHasAggregate(e sqlp.Expr) bool {
	found := false
	_, _ = sqlp.Walk(visitFunc(func(n sqlp.Node) error {
		if c, ok := n.(*sqlp.Call); ok {
			if aggregateFunctions[strings.ToLower(c.Name.Name)] || c.Over != nil {
				found = true
			}
		}
		return nil
	}), e)
	return found
}

// clampLimit injects LIMIT MaxRows when absent and clamps larger literals.
// Non-literal limits pass through; Execute's scan cap covers them.
func clampLimit(s *sqlp.SelectStatement) {
	if s.LimitExpr == nil {
		s.LimitExpr = &sqlp.NumberLit{Value: strconv.Itoa(MaxRows)}
		return
	}
	if lit, ok := s.LimitExpr.(*sqlp.NumberLit); ok {
		if n, err := strconv.Atoi(lit.Value); err == nil && n > MaxRows {
			lit.Value = strconv.Itoa(MaxRows)
		}
	}
}
