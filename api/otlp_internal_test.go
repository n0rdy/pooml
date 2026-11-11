package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

// With every decode slot taken, a request must give up after a bounded wait
// (and get a retryable 429), not sit until the exporter hangs up.
func TestDecodeOTLPGivesUpWhenBusy(t *testing.T) {
	for range cap(otlpDecodeSem) {
		otlpDecodeSem <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(otlpDecodeSem) {
			<-otlpDecodeSem
		}
	})

	start := time.Now()
	_, err := (&Router{}).decodeOTLP(context.Background(), []byte("{}"), true)
	waited := time.Since(start)
	if !errors.Is(err, errOTLPBusy) {
		t.Fatalf("err = %v, want errOTLPBusy", err)
	}
	if waited < otlpDecodeWait || waited > otlpDecodeWait+2*time.Second {
		t.Fatalf("gave up after %v, want about %v", waited, otlpDecodeWait)
	}
}
