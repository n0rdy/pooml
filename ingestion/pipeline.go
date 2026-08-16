package ingestion

import (
	"database/sql"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/n0rdy/pooml/common"

	"github.com/rs/zerolog/log"
)

// Channel sizes and batch bounds: see CONTEXT.md > Ingestion for the rationale
// and the backpressure model built on them.
const (
	ingestChanSize     = 1000
	parsedChanSize     = 5000
	batchInputChanSize = 1000
	batchSize          = 1000
	batchTimeout       = 100 * time.Millisecond

	// entry-count bounds alone allow 1000 x 2 MiB = 2 GiB of buffered
	// payloads - an OOM on the small boxes pooml targets. Bytes are capped
	// too; crossing the budget 429s exactly like a full channel.
	maxBufferedBytes = 256 << 20
)

const insertPrefix = "INSERT INTO logs(timestamp,ingested_at,level,service,host,message,parsed,raw) VALUES "

type ingestEvent struct {
	service    string
	host       string
	payload    []byte
	receivedAt int64
}

// Pipeline is the log ingestion path: HTTP handler -> ingestChan -> parser
// workers -> parsedChan -> fan-out -> (broadcaster, batchInputChan) -> batch
// writer -> logs.db. See CONTEXT.md > Ingestion.
type Pipeline struct {
	logsWrite   *sql.DB
	broadcaster *Broadcaster

	ingestChan     chan ingestEvent
	parsedChan     chan common.StandardLog
	batchInputChan chan common.StandardLog

	parserWG sync.WaitGroup
	fanoutWG sync.WaitGroup
	writerWG sync.WaitGroup

	bufferedBytes atomic.Int64 // payload bytes admitted but not yet parsed

	// closed guards TryIngest against send-on-closed-channel if a shutdown
	// deadline expires while an ingest handler is still running: RLock is
	// held across the send, so Shutdown's Lock waits out in-flight sends.
	closeMu sync.RWMutex
	closed  bool
}

func NewPipeline(logsWrite *sql.DB, broadcaster *Broadcaster) *Pipeline {
	return &Pipeline{
		logsWrite:      logsWrite,
		broadcaster:    broadcaster,
		ingestChan:     make(chan ingestEvent, ingestChanSize),
		parsedChan:     make(chan common.StandardLog, parsedChanSize),
		batchInputChan: make(chan common.StandardLog, batchInputChanSize),
	}
}

func (p *Pipeline) Start() {
	workers := runtime.NumCPU()
	p.parserWG.Add(workers)
	for range workers {
		go p.parserWorker()
	}
	p.fanoutWG.Add(1)
	go p.fanout()
	p.writerWG.Add(1)
	go p.batchWriter()
}

// TryIngest pushes a raw payload onto the pipeline without blocking. False
// means the pipeline is saturated: the caller returns 429 and the client
// (FluentBit) retries with backoff.
func (p *Pipeline) TryIngest(service, host string, payload []byte, receivedAt int64) bool {
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closed {
		return false
	}
	n := int64(len(payload))
	if p.bufferedBytes.Add(n) > maxBufferedBytes {
		p.bufferedBytes.Add(-n)
		return false
	}
	select {
	case p.ingestChan <- ingestEvent{service: service, host: host, payload: payload, receivedAt: receivedAt}:
		return true
	default:
		p.bufferedBytes.Add(-n)
		return false
	}
}

// Shutdown drains the pipeline stage by stage via channel close, in
// producer-to-consumer order, so the batch writer flushes everything that made
// it in. Callers must guarantee no TryIngest calls after this starts (HTTP
// servers are already down in main's shutdown sequence).
func (p *Pipeline) Shutdown() {
	p.closeMu.Lock()
	p.closed = true
	p.closeMu.Unlock()
	close(p.ingestChan)
	p.parserWG.Wait()
	close(p.parsedChan)
	p.fanoutWG.Wait()
	close(p.batchInputChan)
	p.writerWG.Wait()
}

func (p *Pipeline) parserWorker() {
	defer p.parserWG.Done()
	for ev := range p.ingestChan {
		for _, l := range parsePayload(ev.service, ev.host, ev.payload, ev.receivedAt) {
			l.Timestamp = common.ClampTimestamp(l.Timestamp, ev.receivedAt)
			// blocking send on purpose: full parsedChan cascades backpressure
			// to ingestChan and ultimately to 429s
			p.parsedChan <- l
		}
		p.bufferedBytes.Add(-int64(len(ev.payload)))
	}
}

func (p *Pipeline) fanout() {
	defer p.fanoutWG.Done()
	for l := range p.parsedChan {
		// broadcast first for live-tail latency; persistence follows
		p.broadcaster.broadcast(l)
		p.batchInputChan <- l
	}
}

func (p *Pipeline) batchWriter() {
	defer p.writerWG.Done()

	buf := make([]common.StandardLog, 0, batchSize)
	timer := time.NewTimer(batchTimeout)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false

	tryFlush := func() {
		if len(buf) == 0 {
			return
		}
		if err := p.flushBatch(buf); err != nil {
			log.Error().Err(err).Int("rows", len(buf)).Msg("batch write failed, will retry")
			return
		}
		buf = buf[:0]
	}
	drain := func() {
		tryFlush()
		if len(buf) > 0 {
			log.Error().Int("rows", len(buf)).Msg("dropping unflushed rows at shutdown")
		}
	}

	for {
		if len(buf) >= batchSize {
			tryFlush()
			if len(buf) >= batchSize {
				// still failing: back off without pulling more, so the
				// channels fill and backpressure reaches the HTTP layer
				time.Sleep(batchTimeout)
			}
			continue
		}

		if !armed {
			l, ok := <-p.batchInputChan
			if !ok {
				drain()
				return
			}
			buf = append(buf, l)
			timer.Reset(batchTimeout)
			armed = true
			continue
		}

		select {
		case l, ok := <-p.batchInputChan:
			if !ok {
				drain()
				return
			}
			buf = append(buf, l)
		case <-timer.C:
			tryFlush()
			if len(buf) > 0 {
				timer.Reset(batchTimeout) // flush failed: keep retrying on the timer
			} else {
				armed = false
			}
		}
	}
}

func (p *Pipeline) flushBatch(buf []common.StandardLog) error {
	// one multi-row INSERT = one implicit transaction; no explicit tx needed
	q := insertPrefix + strings.TrimSuffix(strings.Repeat("(?,?,?,?,?,?,?,?),", len(buf)), ",")
	args := make([]any, 0, len(buf)*8)
	for i := range buf {
		l := &buf[i]
		args = append(args, l.Timestamp, l.IngestedAt, l.Level, l.Service, l.Host, l.Message, l.Parsed, l.Raw)
	}
	_, err := p.logsWrite.Exec(q, args...)
	return err
}
