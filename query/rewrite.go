package query

import (
	"fmt"
	"strconv"
	"strings"

	sqlp "github.com/rqlite/sql"
)

// Rewrites graft subtrees parsed from template SQL instead of hand-building
// AST nodes: several String() methods key off keyword Pos fields (e.g. the
// bare `*` column, ASC/DESC) that only the parser fills in correctly.

func mustParse(q string) *sqlp.SelectStatement {
	stmt, err := sqlp.NewParser(strings.NewReader(q)).ParseStatement()
	if err != nil {
		panic(fmt.Sprintf("query template %q: %v", q, err))
	}
	return stmt.(*sqlp.SelectStatement)
}

// CombineFTS joins logs_fts and ANDs/ORs `logs_fts.raw MATCH ?` into WHERE.
// The FTS text itself is bound as an argument at execution time, never
// interpolated. Log-viewer shape only; see CONTEXT.md > FTS + SQL Combination.
func (v *Validated) CombineFTS(op string) error {
	if v.Shape != ShapeLogViewer || v.Compound {
		return ErrUnsupportedShape
	}
	if v.stmt.Source == nil {
		return ErrUnsupportedShape
	}

	if !v.referencesFTS() {
		// "rowid" quoted: the parser rejects it as a bare identifier
		join := mustParse(`SELECT * FROM logs JOIN logs_fts ON logs.id = logs_fts."rowid"`).Source.(*sqlp.JoinClause)
		join.X = v.stmt.Source
		v.stmt.Source = join

		// bare * would now expand logs_fts's columns too, adding a duplicate
		// raw column and breaking log-viewer shape detection downstream
		if len(v.stmt.Columns) == 1 && v.stmt.Columns[0].Star.IsValid() {
			v.stmt.Columns = mustParse("SELECT logs.* FROM logs").Columns
		}
	}

	match := mustParse("SELECT * FROM logs_fts WHERE logs_fts.raw MATCH ?").WhereExpr
	tok := sqlp.AND
	if strings.EqualFold(op, "or") {
		tok = sqlp.OR
	}
	if v.stmt.WhereExpr == nil {
		v.stmt.WhereExpr = match
	} else {
		v.stmt.WhereExpr = &sqlp.BinaryExpr{X: match, Op: tok, Y: paren(v.stmt.WhereExpr)}
	}
	return nil
}

// ApplyQuickFilter merges an equality on service/level/host into the query.
// Idempotent and retargeting: if an equality on the same column already
// exists as an AND-conjunct (i.e. one we previously added, or an equivalent
// hand-written one), its value is REPLACED instead of stacking another AND -
// repeated clicks don't grow the query, and clicking service B after
// service A retargets rather than producing a contradiction.
// On aggregation queries it drills in: SELECT * with grouping dropped, so a
// click on an aggregated row shows the underlying logs.
func (v *Validated) ApplyQuickFilter(column, value string) error {
	if v.Compound {
		return ErrUnsupportedShape
	}

	var lit sqlp.Expr
	switch column {
	case "service", "host":
		lit = &sqlp.StringLit{Value: value} // StringLit.String() escapes quotes
	case "level":
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("level filter value %q is not a number", value)
		}
		lit = &sqlp.NumberLit{Value: value}
	default:
		return fmt.Errorf("unsupported filter column %q", column)
	}

	if v.Shape == ShapeAggregation {
		tpl := mustParse("SELECT * FROM logs ORDER BY timestamp ASC LIMIT 50")
		v.stmt.Columns = tpl.Columns
		v.stmt.GroupByExprs = nil
		v.stmt.HavingExpr = nil
		v.stmt.Windows = nil
		v.stmt.Distinct = sqlp.Pos{}
		v.stmt.OrderingTerms = tpl.OrderingTerms
		v.stmt.LimitExpr = tpl.LimitExpr
		v.Shape = ShapeLogViewer
	}

	if existing := findConjunctEq(v.stmt.WhereExpr, column); existing != nil {
		existing.Y = lit
		return nil
	}
	cond := &sqlp.BinaryExpr{X: &sqlp.Ident{Name: column}, Op: sqlp.EQ, Y: lit}
	if v.stmt.WhereExpr == nil {
		v.stmt.WhereExpr = cond
	} else {
		v.stmt.WhereExpr = &sqlp.BinaryExpr{X: paren(v.stmt.WhereExpr), Op: sqlp.AND, Y: cond}
	}
	return nil
}

// HasConditions reports whether the query has a WHERE clause; live tail uses
// it to pick the broadcaster fast path (strategy A) for unfiltered queries.
func (v *Validated) HasConditions() bool { return v.stmt.WhereExpr != nil }

// ApplyStreamCursor rewrites for live-tail polling (strategy B): appends
// `AND id > ?` with a bind parameter so one query shape serves every poll.
// Ordered by id ASC: live tail follows arrival order, not event time.
func (v *Validated) ApplyStreamCursor(limit int) error {
	if v.Shape != ShapeLogViewer || v.Compound {
		return ErrUnsupportedShape
	}
	cond := &sqlp.BinaryExpr{X: &sqlp.Ident{Name: "id"}, Op: sqlp.GT, Y: &sqlp.BindExpr{Name: "?"}}
	if v.stmt.WhereExpr == nil {
		v.stmt.WhereExpr = cond
	} else {
		v.stmt.WhereExpr = &sqlp.BinaryExpr{X: paren(v.stmt.WhereExpr), Op: sqlp.AND, Y: cond}
	}
	tpl := mustParse("SELECT * FROM logs ORDER BY id ASC")
	v.stmt.OrderingTerms = tpl.OrderingTerms
	v.stmt.LimitExpr = &sqlp.NumberLit{Value: strconv.Itoa(limit)}
	v.stmt.OffsetExpr = nil
	return nil
}

// findConjunctEq walks only AND-chains and parens (never into OR/NOT
// branches, where replacing would change hand-written semantics) looking for
// `col = literal`.
func findConjunctEq(e sqlp.Expr, col string) *sqlp.BinaryExpr {
	switch t := e.(type) {
	case *sqlp.ParenExpr:
		return findConjunctEq(t.X, col)
	case *sqlp.BinaryExpr:
		if t.Op == sqlp.AND {
			if r := findConjunctEq(t.X, col); r != nil {
				return r
			}
			return findConjunctEq(t.Y, col)
		}
		if t.Op == sqlp.EQ && eqColName(t.X) == col {
			switch t.Y.(type) {
			case *sqlp.StringLit, *sqlp.NumberLit:
				return t
			}
		}
	}
	return nil
}

func eqColName(e sqlp.Expr) string {
	switch t := e.(type) {
	case *sqlp.Ident:
		return strings.ToLower(t.Name)
	case *sqlp.QualifiedRef:
		if t.Column != nil {
			return strings.ToLower(t.Column.Name)
		}
	}
	return ""
}

// ApplyPagination rewrites for the infinite-scroll cursor. The cursor is the
// composite (timestamp, id): the viewer orders by event time, and event time
// does not follow ingestion order (CLF embedded times, backfills), so a bare
// id cursor would page inconsistently. id breaks timestamp ties.
func (v *Validated) ApplyPagination(beforeTs, beforeID int64, pageSize int) error {
	if v.Shape != ShapeLogViewer || v.Compound {
		return ErrUnsupportedShape
	}
	if pageSize <= 0 || pageSize > MaxRows {
		return fmt.Errorf("page size %d out of range", pageSize)
	}

	ts := strconv.FormatInt(beforeTs, 10)
	lt := func(col, val string) sqlp.Expr {
		return &sqlp.BinaryExpr{X: &sqlp.Ident{Name: col}, Op: sqlp.LT, Y: &sqlp.NumberLit{Value: val}}
	}
	tsEq := &sqlp.BinaryExpr{X: &sqlp.Ident{Name: "timestamp"}, Op: sqlp.EQ, Y: &sqlp.NumberLit{Value: ts}}
	cond := paren(&sqlp.BinaryExpr{
		X:  lt("timestamp", ts),
		Op: sqlp.OR,
		Y: paren(&sqlp.BinaryExpr{
			X:  tsEq,
			Op: sqlp.AND,
			Y:  lt("id", strconv.FormatInt(beforeID, 10)),
		}),
	})

	if v.stmt.WhereExpr == nil {
		v.stmt.WhereExpr = cond
	} else {
		v.stmt.WhereExpr = &sqlp.BinaryExpr{X: paren(v.stmt.WhereExpr), Op: sqlp.AND, Y: cond}
	}

	tpl := mustParse("SELECT * FROM logs ORDER BY timestamp DESC, id DESC")
	v.stmt.OrderingTerms = tpl.OrderingTerms
	v.stmt.LimitExpr = &sqlp.NumberLit{Value: strconv.Itoa(pageSize)}
	v.stmt.OffsetExpr = nil
	return nil
}

func (v *Validated) referencesFTS() bool {
	found := false
	if v.stmt.Source != nil {
		_, _ = sqlp.Walk(visitFunc(func(n sqlp.Node) error {
			if t, ok := n.(*sqlp.QualifiedTableName); ok && strings.EqualFold(t.Name.Name, "logs_fts") {
				found = true
			}
			return nil
		}), v.stmt.Source)
	}
	return found
}

func paren(e sqlp.Expr) sqlp.Expr {
	// don't double-wrap
	if _, ok := e.(*sqlp.ParenExpr); ok {
		return e
	}
	return &sqlp.ParenExpr{X: e}
}
