package query

import (
	"regexp"
	"strings"

	sqlp "github.com/rqlite/sql"
)

var bareIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// prettify strips the identifier quotes the serializer puts on everything
// (`FROM "logs"` -> `FROM logs`), so quick-filter-merged SQL pushed back into
// the editor looks like something a human wrote. An identifier keeps its
// quotes when the bare form would not round-trip: keyword collisions (rowid,
// order, ...) or non-bare characters. String literals are untouched - this
// re-lexes rather than string-replaces precisely so `'he said "hi"'` survives.
func prettify(q string) string {
	type span struct{ start, end int }
	var spans []span

	s := sqlp.NewScanner(strings.NewReader(q))
	for {
		pos, tok, lit := s.Scan()
		if tok == sqlp.EOF || tok == sqlp.ILLEGAL {
			break
		}
		if tok != sqlp.QIDENT || !bareIdentRe.MatchString(lit) || sqlp.Lookup(lit) != sqlp.IDENT {
			continue
		}
		start, end := pos.Offset, pos.Offset+len(lit)+2
		// bail on any offset surprise rather than corrupt the query
		if start < 0 || end > len(q) || q[start] != '"' || q[end-1] != '"' {
			return q
		}
		spans = append(spans, span{start, end})
	}
	if len(spans) == 0 {
		return q
	}

	var b strings.Builder
	b.Grow(len(q))
	prev := 0
	for _, sp := range spans {
		b.WriteString(q[prev:sp.start])
		b.WriteString(q[sp.start+1 : sp.end-1])
		prev = sp.end
	}
	b.WriteString(q[prev:])
	return b.String()
}
