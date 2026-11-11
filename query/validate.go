// Package query is the validation, rewrite, and execution engine for
// user-supplied SQL. See CONTEXT.md > Querying.
package query

import (
	"errors"
	"fmt"
	"slices"
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
	stmt         *sqlp.SelectStatement
	Shape        Shape
	Compound     bool // UNION/INTERSECT/EXCEPT: executable, but rewrites refuse it
	ReadsLogs    bool
	ReadsMetrics bool
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

	cv := &checkVisitor{referenced: map[string]bool{}}
	if _, err := sqlp.Walk(cv, sel); err != nil {
		return nil, err
	}
	readsLogs := cv.referenced["logs"] || cv.referenced["logs_fts"]
	readsMetrics := cv.referenced["metrics"]
	if err := checkScope(scope, readsLogs, readsMetrics); err != nil {
		return nil, err
	}

	v := &Validated{
		stmt:         sel,
		Shape:        detectShape(sel),
		Compound:     sel.Compound != nil,
		ReadsLogs:    readsLogs,
		ReadsMetrics: readsMetrics,
	}
	clampLimit(sel)
	return v, nil
}

func checkScope(scope Scope, readsLogs, readsMetrics bool) error {
	switch scope {
	case ScopeLogs:
		if readsMetrics {
			return errors.New("the metrics table is not available here: query it on the Metrics page")
		}
	case ScopeMetrics:
		if readsLogs {
			return errors.New("the logs tables are not available here: query them on the Logs page")
		}
	}
	return nil
}

// The library's Walk does NOT descend into CTE bodies (WithClause), subqueries
// in expressions (SelectExpr), or the operand of IS NULL / NOTNULL (Null), so
// all three recurse manually here. More traps: CONTEXT.md > Parser traps.
type checkVisitor struct {
	// CTE names visible at the current node, one frame per enclosing SELECT.
	// A global set would let a CTE declared in a subquery whitelist the same
	// name everywhere else in the statement.
	scopes     []map[string]bool
	defining   []cteRef        // CTEs whose bodies are being walked
	referenced map[string]bool // real tables seen; feeds the Scope check
}

// cteRef names a CTE by the frame that declares it, so an inner CTE reusing an
// outer name reads as shadowing, not as recursion.
type cteRef struct {
	name  string
	frame int
}

// asciiLower folds like SQLite does. strings.ToLower also maps U+0130 to "i"
// and the Kelvin sign to "k", so a CTE named with one of them would shadow a
// real table for the validator but not for SQLite.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

func (c *checkVisitor) Visit(n sqlp.Node) (sqlp.Visitor, sqlp.Node, error) {
	switch t := n.(type) {
	case *sqlp.SelectStatement:
		frame := map[string]bool{}
		if t.WithClause != nil {
			for _, cte := range t.WithClause.CTEs {
				frame[asciiLower(cte.TableName.Name)] = true
			}
		}
		c.scopes = append(c.scopes, frame)
	case *sqlp.WithClause:
		if t.Recursive.IsValid() {
			return nil, nil, errRecursiveCTE
		}
		for _, cte := range t.CTEs {
			// the frame on top is the SELECT this WITH belongs to
			c.defining = append(c.defining, cteRef{asciiLower(cte.TableName.Name), len(c.scopes) - 1})
			_, err := sqlp.Walk(c, cte.Select)
			c.defining = c.defining[:len(c.defining)-1]
			if err != nil {
				return nil, nil, err
			}
		}
	case sqlp.SelectExpr:
		if _, err := sqlp.Walk(c, t.SelectStatement); err != nil {
			return nil, nil, err
		}
	case *sqlp.Null:
		if t.X != nil {
			if _, err := sqlp.Walk(c, t.X); err != nil {
				return nil, nil, err
			}
		}
	case *sqlp.QualifiedTableName:
		if t.Schema != nil {
			return nil, nil, fmt.Errorf("schema-qualified table %q is not allowed", t.Schema.Name+"."+t.Name.Name)
		}
		if err := c.tableRef(t.Name.Name); err != nil {
			return nil, nil, err
		}
	case *sqlp.QualifiedTableFunctionName:
		// table-valued functions (pragma_table_info, json_each, ...) would
		// bypass the FROM allow-list and the scope check entirely
		return nil, nil, fmt.Errorf("table-valued function %q is not allowed", t.Name.Name)
	case *sqlp.BinaryExpr:
		// `x IN name` reads a table without a FROM: the parser hands the
		// right side back as a bare Ident / Call / QualifiedRef
		if t.Op == sqlp.IN || t.Op == sqlp.NOTIN {
			switch y := t.Y.(type) {
			case *sqlp.Ident:
				if err := c.tableRef(y.Name); err != nil {
					return nil, nil, err
				}
			case *sqlp.Call:
				return nil, nil, fmt.Errorf("table-valued function %q is not allowed", y.Name.Name)
			case *sqlp.QualifiedRef:
				return nil, nil, errors.New("schema-qualified tables are not allowed")
			}
		}
	case *sqlp.Call:
		if bannedFunctions[strings.ToLower(t.Name.Name)] {
			return nil, nil, fmt.Errorf("function %q is not allowed", t.Name.Name)
		}
	case *sqlp.BindExpr:
		// nothing can supply its value, and it would capture the arguments the
		// rewrites bind afterwards (the search text, the live-tail cursor)
		return nil, nil, fmt.Errorf("placeholder %q is not supported: write the value into the query", t.Name)
	}
	return c, n, nil
}

func (c *checkVisitor) VisitEnd(n sqlp.Node) (sqlp.Node, error) {
	if _, ok := n.(*sqlp.SelectStatement); ok {
		c.scopes = c.scopes[:len(c.scopes)-1]
	}
	return n, nil
}

// SQLite runs a self-referencing CTE recursively even without the RECURSIVE
// keyword, so the ban has to catch the self-reference itself.
var errRecursiveCTE = errors.New("WITH RECURSIVE is not allowed: recursive CTEs can run unbounded")

func (c *checkVisitor) tableRef(name string) error {
	lower := asciiLower(name)
	// innermost declaration wins, as in SQL
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if c.scopes[i][lower] {
			if slices.Contains(c.defining, cteRef{lower, i}) {
				return errRecursiveCTE
			}
			return nil
		}
	}
	if !allowedTables[lower] {
		return fmt.Errorf("table %q is not allowed: only logs, logs_fts and metrics can be queried", name)
	}
	c.referenced[lower] = true
	return nil
}

// visitFunc adapts a plain callback to the Visitor interface.
type visitFunc func(sqlp.Node) error

func (f visitFunc) Visit(n sqlp.Node) (sqlp.Visitor, sqlp.Node, error) {
	if err := f(n); err != nil {
		return nil, nil, err
	}
	// the library's Walk skips the operand of IS NULL / NOTNULL
	if nl, ok := n.(*sqlp.Null); ok && nl.X != nil {
		if _, err := sqlp.Walk(f, nl.X); err != nil {
			return nil, nil, err
		}
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
