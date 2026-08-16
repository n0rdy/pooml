package ingestion

import "testing"

// no Start(): workers must not drain, so admission control alone decides
func TestIngestByteBudget(t *testing.T) {
	p := NewPipeline(nil, NewBroadcaster())
	payload := make([]byte, 2<<20) // the max request size

	accepted := 0
	for range 1000 { // entry-count cap alone would admit all of these
		if !p.TryIngest("svc", "h", payload, 1) {
			break
		}
		accepted++
	}
	want := maxBufferedBytes / (2 << 20)
	if accepted != want {
		t.Errorf("accepted %d payloads of 2 MiB, want %d (byte budget)", accepted, want)
	}

	// draining one event frees its bytes and admission resumes
	ev := <-p.ingestChan
	p.bufferedBytes.Add(-int64(len(ev.payload)))
	if !p.TryIngest("svc", "h", payload, 1) {
		t.Error("budget not released after drain")
	}
}
