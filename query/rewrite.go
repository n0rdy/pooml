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

// ApplyQuickFilter ANDs an equality on service/level/host into the query.
// On aggregation queries it drills in: SELECT * with grouping dropped, so a
// click on an aggregated row shows the underlying logs.
func (v *Validated) ApplyQuickFilter(column, value string) error {
	if v.Compound {
		return ErrUnsupportedShape
	}

	var cond sqlp.Expr
	switch column {
	case "service", "host":
		cond = &sqlp.BinaryExpr{
			X:  &sqlp.Ident{Name: column},
			Op: sqlp.EQ,
			Y:  &sqlp.StringLit{Value: value}, // StringLit.String() escapes quotes
		}
	case "level":
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("level filter value %q is not a number", value)
		}
		cond = &sqlp.BinaryExpr{X: &sqlp.Ident{Name: column}, Op: sqlp.EQ, Y: &sqlp.NumberLit{Value: value}}
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

	if v.stmt.WhereExpr == nil {
		v.stmt.WhereExpr = cond
	} else {
		v.stmt.WhereExpr = &sqlp.BinaryExpr{X: paren(v.stmt.WhereExpr), Op: sqlp.AND, Y: cond}
	}
	return nil
}

// ApplyPagination rewrites for the infinite-scroll cursor: older-than beforeID,
// newest of that window first. See CONTEXT.md > Querying > Pagination.
func (v *Validated) ApplyPagination(beforeID int64, pageSize int) error {
	if v.Shape != ShapeLogViewer || v.Compound {
		return ErrUnsupportedShape
	}
	if pageSize <= 0 || pageSize > MaxRows {
		return fmt.Errorf("page size %d out of range", pageSize)
	}

	cond := &sqlp.BinaryExpr{
		X:  &sqlp.Ident{Name: "id"},
		Op: sqlp.LT,
		Y:  &sqlp.NumberLit{Value: strconv.FormatInt(beforeID, 10)},
	}
	if v.stmt.WhereExpr == nil {
		v.stmt.WhereExpr = cond
	} else {
		v.stmt.WhereExpr = &sqlp.BinaryExpr{X: paren(v.stmt.WhereExpr), Op: sqlp.AND, Y: cond}
	}

	tpl := mustParse("SELECT * FROM logs ORDER BY id DESC")
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
