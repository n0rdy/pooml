package query

import (
	"context"
	"database/sql"
	"time"
)

// Timeout kills runaway queries via QueryContext; mattn cancels the running
// statement through sqlite3_interrupt.
const Timeout = 10 * time.Second

type Result struct {
	Columns   []string
	Rows      [][]any
	Truncated bool // hit the MaxRows scan cap
}

// Execute runs q on the read pool with the query timeout and the MaxRows scan
// cap. []byte columns are converted to string so results render and marshal
// cleanly.
func Execute(ctx context.Context, db *sql.DB, q string, args ...any) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	res := &Result{Columns: cols}
	for rows.Next() {
		if len(res.Rows) >= MaxRows {
			res.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}
