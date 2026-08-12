// Spike: what does FTS5 actually cost on the ingest path?
// Inserts the same generated corpus into two DBs with the real logs schema,
// one with logs_fts + sync triggers and one without, using the production
// batching pattern (multi-row INSERT, one tx per 1000 rows). Measures insert
// throughput, disk size, retention-style chunked deletes, and MATCH vs LIKE.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	batchSize = 1000
	needle    = "OutOfMemoryError"
)

const schemaBase = `
CREATE TABLE logs (
    id INTEGER PRIMARY KEY,
    timestamp INTEGER NOT NULL,
    ingested_at INTEGER NOT NULL,
    level INTEGER,
    service TEXT NOT NULL,
    host TEXT NOT NULL,
    message TEXT,
    parsed TEXT,
    raw TEXT NOT NULL
);
CREATE INDEX idx_timestamp ON logs(timestamp DESC);
CREATE INDEX idx_level ON logs(level, timestamp DESC) WHERE level IS NOT NULL;
CREATE INDEX idx_service ON logs(service, timestamp DESC);
CREATE INDEX idx_host ON logs(host, timestamp DESC);
`

const schemaFTS = `
CREATE VIRTUAL TABLE logs_fts USING fts5(raw, content=logs, content_rowid=id);
CREATE TRIGGER logs_ai AFTER INSERT ON logs BEGIN
    INSERT INTO logs_fts(rowid, raw) VALUES (new.id, new.raw);
END;
CREATE TRIGGER logs_ad AFTER DELETE ON logs BEGIN
    INSERT INTO logs_fts(logs_fts, rowid, raw) VALUES('delete', old.id, old.raw);
END;
CREATE TRIGGER logs_au AFTER UPDATE ON logs BEGIN
    INSERT INTO logs_fts(logs_fts, rowid, raw) VALUES('delete', old.id, old.raw);
    INSERT INTO logs_fts(rowid, raw) VALUES (new.id, new.raw);
END;
`

type logRow struct {
	ts      int64
	level   any
	service string
	host    string
	message any
	parsed  any
	raw     string
}

func main() {
	rows := flag.Int("rows", 1_000_000, "corpus size")
	random := flag.Bool("random", false, "leave timestamps in random order (pessimistic; real arrival is near-append)")
	flag.Parse()

	dir, err := os.MkdirTemp("", "pooml-ftsbench-*")
	check(err)
	defer os.RemoveAll(dir)

	fmt.Printf("generating %d-row corpus...\n", *rows)
	corpus := genCorpus(*rows)
	if !*random {
		sort.Slice(corpus, func(i, j int) bool { return corpus[i].ts < corpus[j].ts })
	}
	var rawBytes int64
	for i := range corpus {
		rawBytes += int64(len(corpus[i].raw))
	}
	fmt.Printf("corpus: %d rows, avg raw line %d B\n\n", *rows, rawBytes/int64(*rows))

	type result struct {
		name                 string
		insertDur, deleteDur time.Duration
		sizeMB               float64
	}
	var results []result

	for _, cfg := range []struct {
		name   string
		schema string
	}{
		{"no-fts", schemaBase},
		{"with-fts", schemaBase + schemaFTS},
	} {
		path := filepath.Join(dir, cfg.name+".db")
		db := open(path, cfg.schema)

		insertDur := insertAll(db, corpus)
		if cfg.name == "with-fts" {
			benchQueries(db)
		} else {
			benchLike(db)
		}
		deleteDur := chunkDelete(db, *rows/10)
		checkpoint(db)
		results = append(results, result{cfg.name, insertDur, deleteDur, fileMB(path)})
		check(db.Close())
	}

	fmt.Printf("\n%-10s %14s %12s %14s %10s\n", "schema", "insert", "rows/sec", "del 10% rows", "size MB")
	for _, r := range results {
		fmt.Printf("%-10s %14s %12.0f %14s %10.1f\n",
			r.name, r.insertDur.Round(time.Millisecond),
			float64(*rows)/r.insertDur.Seconds(),
			r.deleteDur.Round(time.Millisecond), r.sizeMB)
	}
	a, b := results[0], results[1]
	fmt.Printf("\nFTS overhead: %.0f%% slower inserts, %.1fx disk (index adds %.1f MB for %.1f MB of raw text)\n",
		(b.insertDur.Seconds()/a.insertDur.Seconds()-1)*100,
		b.sizeMB/a.sizeMB, b.sizeMB-a.sizeMB, float64(rawBytes)/1024/1024)
}

func open(path, schema string) *sql.DB {
	db, err := sql.Open("sqlite3", "file:"+path)
	check(err)
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -40000",
	} {
		_, err = db.Exec(p)
		check(err)
	}
	_, err = db.Exec(schema)
	check(err)
	return db
}

func insertAll(db *sql.DB, corpus []logRow) time.Duration {
	placeholder := "(?,?,?,?,?,?,?,?)"
	full, err := db.Prepare(
		"INSERT INTO logs(timestamp,ingested_at,level,service,host,message,parsed,raw) VALUES " +
			strings.TrimSuffix(strings.Repeat(placeholder+",", batchSize), ","))
	check(err)
	defer full.Close()

	args := make([]any, 0, batchSize*8)
	start := time.Now()
	for off := 0; off < len(corpus); off += batchSize {
		end := min(off+batchSize, len(corpus))
		args = args[:0]
		for _, r := range corpus[off:end] {
			args = append(args, r.ts, r.ts+37, r.level, r.service, r.host, r.message, r.parsed, r.raw)
		}
		tx, err := db.Begin()
		check(err)
		if end-off == batchSize {
			_, err = tx.Stmt(full).Exec(args...)
		} else {
			_, err = tx.Exec(
				"INSERT INTO logs(timestamp,ingested_at,level,service,host,message,parsed,raw) VALUES "+
					strings.TrimSuffix(strings.Repeat(placeholder+",", end-off), ","), args...)
		}
		check(err)
		check(tx.Commit())
	}
	return time.Since(start)
}

// chunkDelete mimics the retention job: delete oldest n rows, 10k per tx.
func chunkDelete(db *sql.DB, n int) time.Duration {
	start := time.Now()
	for deleted := 0; deleted < n; {
		res, err := db.Exec(`DELETE FROM logs WHERE id IN
			(SELECT id FROM logs ORDER BY id LIMIT 10000)`)
		check(err)
		aff, _ := res.RowsAffected()
		if aff == 0 {
			break
		}
		deleted += int(aff)
	}
	return time.Since(start)
}

func benchQueries(db *sql.DB) {
	var cnt int
	start := time.Now()
	check(db.QueryRow("SELECT count(*) FROM logs_fts WHERE logs_fts MATCH ?", needle).Scan(&cnt))
	fmt.Printf("  FTS MATCH %q:  %d hits in %s\n", needle, cnt, time.Since(start).Round(time.Microsecond))
	benchLike(db)
}

func benchLike(db *sql.DB) {
	var cnt int
	start := time.Now()
	check(db.QueryRow("SELECT count(*) FROM logs WHERE raw LIKE ?", "%"+needle+"%").Scan(&cnt))
	fmt.Printf("  LIKE scan %q: %d hits in %s\n", needle, cnt, time.Since(start).Round(time.Microsecond))
}

func checkpoint(db *sql.DB) {
	_, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	check(err)
}

func fileMB(path string) float64 {
	fi, err := os.Stat(path)
	check(err)
	return float64(fi.Size()) / 1024 / 1024
}

func genCorpus(n int) []logRow {
	rng := rand.New(rand.NewSource(42))
	services := []string{"api-gateway", "payment-svc", "auth-svc", "worker", "caddy"}
	hosts := []string{"hetzner-1", "hetzner-2"}
	paths := []string{"/api/v1/users", "/api/v1/orders", "/healthcheck", "/login", "/api/v1/payments", "/static/app.js"}
	agents := []string{"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36", "curl/8.5.0", "Go-http-client/2.0"}
	levels := []string{"debug", "info", "info", "info", "info", "info", "warn", "error"}
	levelNums := map[string]int{"debug": 1, "info": 2, "warn": 3, "error": 4}

	now := time.Now().UnixMilli()
	monthMs := int64(30 * 24 * 3600 * 1000)

	hexID := func() string {
		const chars = "0123456789abcdef"
		b := make([]byte, 12)
		for i := range b {
			b[i] = chars[rng.Intn(16)]
		}
		return string(b)
	}
	ip := func() string {
		return fmt.Sprintf("%d.%d.%d.%d", 10+rng.Intn(200), rng.Intn(255), rng.Intn(255), 1+rng.Intn(254))
	}

	out := make([]logRow, n)
	for i := range out {
		ts := now - rng.Int63n(monthMs)
		svc := services[rng.Intn(len(services))]
		host := hosts[rng.Intn(len(hosts))]

		var r logRow
		switch f := rng.Intn(100); {
		case svc == "caddy" || f < 25: // access log, CLF
			status := []int{200, 200, 200, 200, 301, 404, 500}[rng.Intn(7)]
			r = logRow{
				ts: ts, level: nil, service: svc, host: host, message: nil, parsed: nil,
				raw: fmt.Sprintf(`%s - - [%s] "GET %s HTTP/1.1" %d %d "-" "%s"`,
					ip(), time.UnixMilli(ts).Format("02/Jan/2006:15:04:05 -0700"),
					paths[rng.Intn(len(paths))], status, 100+rng.Intn(50000),
					agents[rng.Intn(len(agents))]),
			}
		case f < 85: // structured JSON app log
			lvl := levels[rng.Intn(len(levels))]
			msg := []string{
				"request completed",
				"cache miss for key user:" + hexID(),
				"user " + hexID() + " authenticated",
				"payment " + hexID() + " settled",
				fmt.Sprintf("slow query detected: %dms", 50+rng.Intn(3000)),
				"retrying operation after transient failure",
			}[rng.Intn(6)]
			if rng.Intn(1000) == 0 {
				msg = "java.lang." + needle + ": heap exhausted while processing " + hexID()
				lvl = "error"
			}
			raw := fmt.Sprintf(`{"level":"%s","ts":%d,"service":"%s","trace_id":"%s","latency_ms":%d,"msg":"%s"}`,
				lvl, ts, svc, hexID(), rng.Intn(500), msg)
			r = logRow{ts: ts, level: levelNums[lvl], service: svc, host: host, message: msg, parsed: raw, raw: raw}
		default: // plain text
			lvl := levels[rng.Intn(len(levels))]
			msg := fmt.Sprintf("Connection established to %s:5432 pool=%d", ip(), rng.Intn(20))
			r = logRow{
				ts: ts, level: levelNums[lvl], service: svc, host: host, message: msg, parsed: nil,
				raw: fmt.Sprintf("%s %s %s", time.UnixMilli(ts).Format(time.RFC3339), strings.ToUpper(lvl), msg),
			}
		}
		out[i] = r
	}
	return out
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
