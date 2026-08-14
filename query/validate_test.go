package query

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateAccepts(t *testing.T) {
	tests := []struct {
		name      string
		q         string
		wantShape Shape
		wantSQL   string // "" = don't assert exact serialization
	}{
		{
			name:      "default editor query",
			q:         "SELECT * FROM logs ORDER BY timestamp DESC LIMIT 100",
			wantShape: ShapeLogViewer,
			wantSQL:   "SELECT * FROM logs ORDER BY timestamp DESC LIMIT 100",
		},
		{
			name:      "missing LIMIT gets injected",
			q:         "SELECT * FROM logs",
			wantShape: ShapeLogViewer,
			wantSQL:   "SELECT * FROM logs LIMIT 10000",
		},
		{
			name:      "oversized LIMIT gets clamped",
			q:         "SELECT * FROM logs LIMIT 50000",
			wantShape: ShapeLogViewer,
			wantSQL:   "SELECT * FROM logs LIMIT 10000",
		},
		{
			name:      "group by is aggregation",
			q:         "SELECT service, COUNT(*) AS cnt FROM logs GROUP BY service HAVING cnt > 10",
			wantShape: ShapeAggregation,
		},
		{
			name:      "metrics table (attached metrics.db)",
			q:         "SELECT name, value FROM metrics WHERE service = 'shop' LIMIT 50",
			wantShape: ShapeLogViewer,
		},
		{
			name:      "cross-DB join of logs and metrics",
			q:         "SELECT l.raw, m.value FROM logs l JOIN metrics m ON l.service = m.service LIMIT 50",
			wantShape: ShapeLogViewer,
		},
		{
			name:      "bare aggregate is aggregation",
			q:         "SELECT count(*) FROM logs",
			wantShape: ShapeAggregation,
		},
		{
			name:      "distinct is aggregation",
			q:         "SELECT DISTINCT service FROM logs",
			wantShape: ShapeAggregation,
		},
		{
			name:      "explicit fts join stays log viewer",
			q:         `SELECT logs.* FROM logs JOIN logs_fts ON logs.id = logs_fts."rowid" WHERE logs_fts.raw MATCH 'boom' LIMIT 10`,
			wantShape: ShapeLogViewer,
		},
		{
			name:      "exists subquery on logs",
			q:         "SELECT 'api-gateway silent' WHERE NOT EXISTS (SELECT 1 FROM logs WHERE service = 'api-gateway' AND timestamp > unixepoch() * 1000 - 600000)",
			wantShape: ShapeLogViewer,
		},
		{
			name:      "non-recursive CTE over logs",
			q:         "WITH errs AS (SELECT * FROM logs WHERE level >= 4) SELECT * FROM errs LIMIT 10",
			wantShape: ShapeLogViewer,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := Validate(tt.q)
			if err != nil {
				t.Fatalf("Validate(%q) = %v", tt.q, err)
			}
			if v.Shape != tt.wantShape {
				t.Errorf("shape = %v, want %v", v.Shape, tt.wantShape)
			}
			if tt.wantSQL != "" && v.SQL() != tt.wantSQL {
				t.Errorf("SQL() = %q, want %q", v.SQL(), tt.wantSQL)
			}
			// serialized form must survive re-validation
			if _, err := Validate(v.SQL()); err != nil {
				t.Errorf("re-Validate(%q) = %v", v.SQL(), err)
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		q       string
		wantErr error  // nil = any error
		wantMsg string // substring
	}{
		{name: "insert", q: "INSERT INTO logs(raw) VALUES ('x')", wantErr: ErrNotSelect},
		{name: "update", q: "UPDATE logs SET raw = 'x'", wantErr: ErrNotSelect},
		{name: "delete", q: "DELETE FROM logs", wantErr: ErrNotSelect},
		{name: "drop", q: "DROP TABLE logs", wantErr: ErrNotSelect},
		{name: "two statements", q: "SELECT 1; SELECT 2", wantErr: ErrMultipleStatements},
		{name: "sqlite_master", q: "SELECT * FROM sqlite_master", wantMsg: "not allowed"},
		{name: "schema-qualified", q: "SELECT * FROM main.logs", wantMsg: "schema-qualified"},
		{name: "forbidden table in subquery", q: "SELECT * FROM logs WHERE id IN (SELECT id FROM api_keys)", wantMsg: "not allowed"},
		{name: "forbidden table behind union", q: "SELECT raw FROM logs UNION SELECT sql FROM sqlite_master", wantMsg: "not allowed"},
		{name: "forbidden table inside CTE body", q: "WITH x AS (SELECT * FROM sqlite_master) SELECT * FROM x", wantMsg: "not allowed"},
		{name: "load_extension", q: "SELECT load_extension('evil.so')", wantMsg: "load_extension"},
		{name: "with recursive", q: "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT * FROM c", wantMsg: "RECURSIVE"},
		{name: "garbage", q: "not sql at all"},
		{name: "empty", q: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Validate(tt.q)
			if err == nil {
				t.Fatalf("Validate(%q) succeeded, want error", tt.q)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %q, want substring %q", err, tt.wantMsg)
			}
		})
	}
}

// Signals are isolated per surface; only ScopeAll (alerts) may mix them.
func TestValidateScopes(t *testing.T) {
	tests := []struct {
		name    string
		q       string
		scope   Scope
		wantErr string // "" = must pass
	}{
		{"logs page: logs ok", "SELECT * FROM logs LIMIT 5", ScopeLogs, ""},
		{"logs page: fts ok", `SELECT * FROM logs WHERE id IN (SELECT "rowid" FROM logs_fts WHERE logs_fts MATCH 'x')`, ScopeLogs, ""},
		{"logs page: metrics blocked", "SELECT * FROM metrics LIMIT 5", ScopeLogs, "Metrics page"},
		{"logs page: join blocked", "SELECT * FROM logs l JOIN metrics m ON l.service = m.service", ScopeLogs, "Metrics page"},
		{"logs page: metrics in subquery blocked", "SELECT * FROM logs WHERE service IN (SELECT service FROM metrics)", ScopeLogs, "Metrics page"},
		{"explorer: metrics ok", "SELECT name, value FROM metrics LIMIT 5", ScopeMetrics, ""},
		{"explorer: logs blocked", "SELECT * FROM logs LIMIT 5", ScopeMetrics, "Logs page"},
		{"explorer: logs in CTE blocked", "WITH x AS (SELECT service FROM logs) SELECT * FROM metrics WHERE service IN (SELECT service FROM x)", ScopeMetrics, "Logs page"},
		{"panel: logs alone ok", "SELECT service, COUNT(*) FROM logs GROUP BY service", ScopeOneSignal, ""},
		{"panel: metrics alone ok", "SELECT timestamp, value FROM metrics ORDER BY timestamp", ScopeOneSignal, ""},
		{"panel: mixing blocked", "SELECT * FROM logs l JOIN metrics m ON l.service = m.service", ScopeOneSignal, "one signal"},
		{"alerts: mixing allowed", "SELECT 1 FROM metrics m WHERE m.value > 10 AND EXISTS (SELECT 1 FROM logs WHERE level >= 4)", ScopeAll, ""},
		{"CTE named like a signal doesn't count as one", "WITH metrics AS (SELECT service FROM logs) SELECT * FROM metrics", ScopeLogs, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateIn(tc.q, tc.scope)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestCombineFTS(t *testing.T) {
	t.Run("default query gains join and match", func(t *testing.T) {
		v, err := Validate("SELECT * FROM logs WHERE level >= 4 ORDER BY timestamp DESC LIMIT 100")
		if err != nil {
			t.Fatal(err)
		}
		if err := v.CombineFTS("and"); err != nil {
			t.Fatal(err)
		}
		want := `SELECT logs.* FROM logs JOIN logs_fts ON logs.id = logs_fts."rowid" WHERE logs_fts.raw MATCH ? AND (level >= 4) ORDER BY timestamp DESC LIMIT 100`
		if v.SQL() != want {
			t.Errorf("SQL() = %q\nwant     %q", v.SQL(), want)
		}
	})

	t.Run("or operator", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs WHERE level >= 4 LIMIT 10")
		if err := v.CombineFTS("or"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(v.SQL(), "MATCH ? OR (level >= 4)") {
			t.Errorf("SQL() = %q", v.SQL())
		}
	})

	t.Run("no user where", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs LIMIT 10")
		if err := v.CombineFTS("and"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(v.SQL(), "WHERE logs_fts.raw MATCH ?") || strings.Contains(v.SQL(), "AND") {
			t.Errorf("SQL() = %q", v.SQL())
		}
	})

	t.Run("existing fts join not duplicated", func(t *testing.T) {
		v, err := Validate(`SELECT logs.* FROM logs JOIN logs_fts ON logs.id = logs_fts."rowid" LIMIT 10`)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.CombineFTS("and"); err != nil {
			t.Fatal(err)
		}
		if strings.Count(v.SQL(), "JOIN logs_fts") != 1 {
			t.Errorf("SQL() = %q, want exactly one join", v.SQL())
		}
	})

	t.Run("bare rowid gets a helpful hint", func(t *testing.T) {
		_, err := Validate("SELECT logs.* FROM logs JOIN logs_fts ON logs.id = logs_fts.rowid LIMIT 10")
		if err == nil || !strings.Contains(err.Error(), "double quotes") {
			t.Errorf("err = %v, want rowid quoting hint", err)
		}
	})

	t.Run("aggregation refused", func(t *testing.T) {
		v, _ := Validate("SELECT service, count(*) FROM logs GROUP BY service")
		if err := v.CombineFTS("and"); !errors.Is(err, ErrUnsupportedShape) {
			t.Errorf("err = %v, want ErrUnsupportedShape", err)
		}
	})
}

func TestApplyQuickFilter(t *testing.T) {
	t.Run("regular query gains AND equality", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs WHERE level >= 3 LIMIT 100")
		if err := v.ApplyQuickFilter("service", "payment-svc"); err != nil {
			t.Fatal(err)
		}
		want := `SELECT * FROM logs WHERE (level >= 3) AND service = 'payment-svc' LIMIT 100`
		if v.SQL() != want {
			t.Errorf("SQL() = %q\nwant     %q", v.SQL(), want)
		}
	})

	t.Run("no where sets equality directly", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs LIMIT 100")
		if err := v.ApplyQuickFilter("level", "4"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(v.SQL(), "WHERE level = 4") {
			t.Errorf("SQL() = %q", v.SQL())
		}
	})

	t.Run("quote in value survives round trip", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs LIMIT 10")
		if err := v.ApplyQuickFilter("service", "o'brien's-svc"); err != nil {
			t.Fatal(err)
		}
		// escaped in serialized SQL and still parseable
		if _, err := Validate(v.SQL()); err != nil {
			t.Fatalf("re-Validate(%q) = %v", v.SQL(), err)
		}
	})

	t.Run("aggregation drills in", func(t *testing.T) {
		v, _ := Validate("SELECT service, COUNT(*) AS cnt FROM logs WHERE level >= 4 GROUP BY service HAVING cnt > 10 ORDER BY cnt DESC")
		if err := v.ApplyQuickFilter("service", "payment-svc"); err != nil {
			t.Fatal(err)
		}
		want := `SELECT * FROM logs WHERE (level >= 4) AND service = 'payment-svc' ORDER BY timestamp ASC LIMIT 50`
		if v.SQL() != want {
			t.Errorf("SQL() = %q\nwant     %q", v.SQL(), want)
		}
		if v.Shape != ShapeLogViewer {
			t.Error("shape should become log viewer after drill-in")
		}
	})

	t.Run("non-numeric level rejected", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs LIMIT 10")
		if err := v.ApplyQuickFilter("level", "4 OR 1=1"); err == nil {
			t.Error("injection-shaped level value accepted")
		}
	})

	t.Run("unknown column rejected", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs LIMIT 10")
		if err := v.ApplyQuickFilter("raw", "x"); err == nil {
			t.Error("unknown filter column accepted")
		}
	})

	t.Run("repeat click is idempotent", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs LIMIT 100")
		for range 3 {
			if err := v.ApplyQuickFilter("service", "payment-svc"); err != nil {
				t.Fatal(err)
			}
		}
		if got := strings.Count(v.SQL(), "service ="); got != 1 {
			t.Errorf("SQL() = %q, want exactly one service condition, got %d", v.SQL(), got)
		}
	})

	t.Run("different value retargets instead of stacking", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs WHERE level >= 3 LIMIT 100")
		if err := v.ApplyQuickFilter("service", "auth-svc"); err != nil {
			t.Fatal(err)
		}
		if err := v.ApplyQuickFilter("service", "payment-svc"); err != nil {
			t.Fatal(err)
		}
		want := `SELECT * FROM logs WHERE (level >= 3) AND service = 'payment-svc' LIMIT 100`
		if v.SQL() != want {
			t.Errorf("SQL() = %q\nwant     %q", v.SQL(), want)
		}
	})

	t.Run("hand-written qualified equality is retargeted", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs WHERE logs.service = 'old' LIMIT 10")
		if err := v.ApplyQuickFilter("service", "new"); err != nil {
			t.Fatal(err)
		}
		if strings.Count(v.SQL(), "'new'") != 1 || strings.Contains(v.SQL(), "'old'") {
			t.Errorf("SQL() = %q", v.SQL())
		}
	})

	t.Run("equality inside OR branch is left alone", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs WHERE service = 'a' OR level = 4 LIMIT 10")
		if err := v.ApplyQuickFilter("service", "b"); err != nil {
			t.Fatal(err)
		}
		// the OR semantics stay; the filter appends as a new conjunct
		if !strings.Contains(v.SQL(), "service = 'a' OR level = 4") || !strings.Contains(v.SQL(), "service = 'b'") {
			t.Errorf("SQL() = %q", v.SQL())
		}
	})

	t.Run("compound refused", func(t *testing.T) {
		v, err := Validate("SELECT raw FROM logs UNION SELECT raw FROM logs")
		if err != nil {
			t.Fatal(err)
		}
		if err := v.ApplyQuickFilter("service", "x"); !errors.Is(err, ErrUnsupportedShape) {
			t.Errorf("err = %v, want ErrUnsupportedShape", err)
		}
	})
}

func TestApplyPagination(t *testing.T) {
	t.Run("composite cursor rewrite", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs WHERE service = 'x' ORDER BY timestamp DESC LIMIT 100")
		if err := v.ApplyPagination(999000, 12345, 50); err != nil {
			t.Fatal(err)
		}
		want := `SELECT * FROM logs WHERE (service = 'x') AND (timestamp < 999000 OR (timestamp = 999000 AND id < 12345)) ORDER BY timestamp DESC, id DESC LIMIT 50`
		if v.SQL() != want {
			t.Errorf("SQL() = %q\nwant     %q", v.SQL(), want)
		}
		if _, err := Validate(v.SQL()); err != nil {
			t.Errorf("re-Validate: %v", err)
		}
	})

	t.Run("aggregation refused", func(t *testing.T) {
		v, _ := Validate("SELECT service, count(*) FROM logs GROUP BY service")
		if err := v.ApplyPagination(1, 10, 50); !errors.Is(err, ErrUnsupportedShape) {
			t.Errorf("err = %v, want ErrUnsupportedShape", err)
		}
	})

	t.Run("bad page size", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs LIMIT 10")
		if err := v.ApplyPagination(1, 10, 0); err == nil {
			t.Error("page size 0 accepted")
		}
	})
}

func TestPrettify(t *testing.T) {
	t.Run("string literal with inner double quotes untouched", func(t *testing.T) {
		v, _ := Validate("SELECT * FROM logs LIMIT 10")
		if err := v.ApplyQuickFilter("service", `he said "hi"`); err != nil {
			t.Fatal(err)
		}
		want := `SELECT * FROM logs WHERE service = 'he said "hi"' LIMIT 10`
		if v.SQL() != want {
			t.Errorf("SQL() = %q\nwant     %q", v.SQL(), want)
		}
		if _, err := Validate(v.SQL()); err != nil {
			t.Errorf("re-Validate: %v", err)
		}
	})

	t.Run("keyword-named identifier keeps quotes", func(t *testing.T) {
		v, err := Validate(`SELECT "order" FROM logs LIMIT 10`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(v.SQL(), `"order"`) {
			t.Errorf("SQL() = %q, keyword ident must stay quoted", v.SQL())
		}
		if _, err := Validate(v.SQL()); err != nil {
			t.Errorf("re-Validate: %v", err)
		}
	})

	t.Run("non-bare identifier keeps quotes", func(t *testing.T) {
		v, err := Validate(`SELECT "weird col" FROM logs LIMIT 10`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(v.SQL(), `"weird col"`) {
			t.Errorf("SQL() = %q", v.SQL())
		}
	})
}
