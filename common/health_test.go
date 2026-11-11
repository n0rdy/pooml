package common

import (
	"errors"
	"testing"
)

func TestWriterHealth(t *testing.T) {
	var nilHealth *WriterHealth
	if since, msg := nilHealth.Failing(); since != 0 || msg != "" {
		t.Fatal("a nil health must read as healthy")
	}

	var h WriterHealth
	if since, _ := h.Failing(); since != 0 {
		t.Fatal("the zero value must be healthy")
	}
	h.Failed(errors.New("disk full"), 1000)
	h.Failed(errors.New("still full"), 2000)
	if since, msg := h.Failing(); since != 1000 || msg != "still full" {
		t.Fatalf("since=%d msg=%q, want the first failure time and the latest error", since, msg)
	}
	h.OK()
	if since, _ := h.Failing(); since != 0 {
		t.Fatal("OK must clear the streak")
	}
	h.Failed(errors.New("again"), 3000)
	if since, _ := h.Failing(); since != 3000 {
		t.Fatalf("a new streak must start at its own time, got %d", since)
	}
}
