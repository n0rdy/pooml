package db

import (
	"errors"

	"github.com/mattn/go-sqlite3"
)

// IsPermanent reports whether a write error will repeat identically on retry
// (the data itself is unacceptable), as opposed to busy/IO/memory conditions.
// Batch writers use it to drop a poison row instead of retrying it forever.
func IsPermanent(err error) bool {
	var se sqlite3.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code {
	case sqlite3.ErrConstraint, sqlite3.ErrTooBig, sqlite3.ErrMismatch, sqlite3.ErrRange:
		return true
	}
	return false
}
