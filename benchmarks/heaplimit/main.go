// Spike: is PRAGMA hard_heap_limit per-connection or process-global?
// CONTEXT.md assumes setting it on the logs read pool caps user queries
// without affecting the write pool. This verifies that assumption.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattn/go-sqlite3"
)

const limit = 104857600 // 100 MB, same as db/pool.go

func main() {
	dir, err := os.MkdirTemp("", "pooml-heaplimit-*")
	check(err)
	defer os.RemoveAll(dir)

	sql.Register("limited", &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		_, err := c.Exec(fmt.Sprintf("PRAGMA hard_heap_limit = %d", limit), nil)
		return err
	}})
	sql.Register("plain", &sqlite3.SQLiteDriver{})

	plainDB, err := sql.Open("plain", "file:"+filepath.Join(dir, "plain.db"))
	check(err)
	plainDB.SetMaxOpenConns(1)
	check(plainDB.Ping())
	fmt.Println("plain pool, before limited pool exists: ", readLimit(plainDB))

	limDB, err := sql.Open("limited", "file:"+filepath.Join(dir, "limited.db"))
	check(err)
	limDB.SetMaxOpenConns(1)
	check(limDB.Ping())
	fmt.Println("limited pool:                            ", readLimit(limDB))
	fmt.Println("plain pool, after limited pool opened:   ", readLimit(plainDB))

	// Functional proof: a ~380 MB group_concat on the PLAIN pool. If the limit
	// leaked process-wide, this fails with out-of-memory despite the plain
	// driver never setting any pragma.
	var s string
	err = plainDB.QueryRow(`
		WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c LIMIT 3000000)
		SELECT group_concat(hex(randomblob(64))) FROM c`).Scan(&s)
	if err != nil {
		fmt.Println("big query on plain pool: ERROR:", err)
	} else {
		fmt.Printf("big query on plain pool: OK, %d MB result\n", len(s)/1024/1024)
	}

	_, err = plainDB.Exec("PRAGMA hard_heap_limit = 0")
	fmt.Println("clearing from plain pool, err:", err)
	fmt.Println("plain pool after clear:   ", readLimit(plainDB))
	fmt.Println("limited pool after clear: ", readLimit(limDB))
}

func readLimit(db *sql.DB) int64 {
	var v int64
	check(db.QueryRow("PRAGMA hard_heap_limit").Scan(&v))
	return v
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
