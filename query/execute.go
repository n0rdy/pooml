package query

import (
	"context"
	"database/sql"
	"time"
)

// Timeout kills runaway queries via QueryContext; mattn cancels the running
// statement through sqlite3_interrupt.
const Timeout = 10 * time.Second

// MaxResultBytes caps the total string/blob bytes a single result set may
// accumulate. MaxRows alone doesn't bound memory: `raw` can be up to the 2 MiB
// ingest cap, so 10K such rows is tens of GB. The cell-width clamp in the query
// API runs only after Execute returns, and export/SSE-refresh/alert-eval apply
// none, so the bound belongs here.
const MaxResultBytes = 64 << 20

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
	var resultBytes int
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
			switch b := v.(type) {
			case []byte:
				resultBytes += len(b)
				vals[i] = string(b)
			case string:
				resultBytes += len(b)
			}
		}
		res.Rows = append(res.Rows, vals)
		if resultBytes >= MaxResultBytes {
			res.Truncated = true
			break
		}
	}
	return res, rows.Err()
}
