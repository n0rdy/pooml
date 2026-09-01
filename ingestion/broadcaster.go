package ingestion

import (
	"sync"

	"github.com/n0rdy/pooml/common"
)

const subscriberBufSize = 100

// Broadcaster fans freshly-parsed logs out to SSE subscribers. Sends never
// block: a slow subscriber drops logs (its problem), ingestion is never held
// up. No recent-log retention: a ring of full StandardLogs would pin up to
// ringSize x the 2 MiB ingest cap of Raw bytes indefinitely, and the stream
// path does no backfill (the page render already shows current state).
type Broadcaster struct {
	mu     sync.Mutex
	subs   map[int]chan common.StandardLog
	nextID int
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[int]chan common.StandardLog)}
}

func (b *Broadcaster) broadcast(l common.StandardLog) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, ch := range b.subs {
		select {
		case ch <- l:
		default:
		}
	}
}

// Subscribe returns a channel of live logs and an unsubscribe func. The
// channel is closed on unsubscribe; the subscriber must stop reading only
// after calling it.
func (b *Broadcaster) Subscribe() (<-chan common.StandardLog, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.nextID
	b.nextID++
	ch := make(chan common.StandardLog, subscriberBufSize)
	b.subs[id] = ch

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(ch)
		}
	}
}
