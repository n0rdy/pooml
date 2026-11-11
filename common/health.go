package common

import "sync/atomic"

// WriterHealth records since when a batch writer has been failing. Nothing
// else can report it: a writer that keeps erroring (a full disk, usually) still
// answers pings, and ingestion keeps returning 204 until the queues fill.
// The zero value is healthy; a nil *WriterHealth reads as healthy too.
type WriterHealth struct {
	failure atomic.Pointer[writerFailure]
}

type writerFailure struct {
	sinceMs int64
	msg     string
}

// Failed keeps the time of the FIRST failure of the streak and the latest error.
func (h *WriterHealth) Failed(err error, nowMs int64) {
	since := nowMs
	if cur := h.failure.Load(); cur != nil {
		since = cur.sinceMs
	}
	h.failure.Store(&writerFailure{sinceMs: since, msg: err.Error()})
}

func (h *WriterHealth) OK() { h.failure.Store(nil) }

// Failing returns 0 and "" while the writer is healthy.
func (h *WriterHealth) Failing() (sinceMs int64, msg string) {
	if h == nil {
		return 0, ""
	}
	if cur := h.failure.Load(); cur != nil {
		return cur.sinceMs, cur.msg
	}
	return 0, ""
}
