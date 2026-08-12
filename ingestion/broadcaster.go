package ingestion

import (
	"sync"

	"github.com/n0rdy/pooml/common"
)

const (
	subscriberBufSize = 100
	ringSize          = 1000
)

// Broadcaster fans freshly-parsed logs out to SSE subscribers and keeps a ring
// of recent logs for backfill on connect. Sends never block: a slow subscriber
// drops logs (its problem), ingestion is never held up.
type Broadcaster struct {
	mu     sync.Mutex
	subs   map[int]chan common.StandardLog
	nextID int
	ring   [ringSize]common.StandardLog
	pos    int
	count  int
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[int]chan common.StandardLog)}
}

func (b *Broadcaster) broadcast(l common.StandardLog) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.ring[b.pos] = l
	b.pos = (b.pos + 1) % ringSize
	if b.count < ringSize {
		b.count++
	}

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

// Backfill returns a copy of the ring buffer, oldest first.
func (b *Broadcaster) Backfill() []common.StandardLog {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]common.StandardLog, 0, b.count)
	start := b.pos - b.count
	if start < 0 {
		start += ringSize
	}
	for i := 0; i < b.count; i++ {
		out = append(out, b.ring[(start+i)%ringSize])
	}
	return out
}
