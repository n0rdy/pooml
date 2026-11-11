package ingestion

import (
	"database/sql"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/n0rdy/pooml/common"
	"github.com/n0rdy/pooml/db"

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

	// The read pool refuses any row over db.MaxReadCellBytes once a query has
	// to sort or materialize it, so one oversized log would fail every search
	// that matches it. Decoding can grow a line well past the 2 MiB payload
	// cap (invalid UTF-8 triples), so the bound is enforced here, per log.
	maxLogRowBytes = db.MaxReadCellBytes / 2
)

// one INSERT binds its whole batch inside SQLite's process-wide 1 GiB heap:
// 1000 rows of 2 MiB lines can never fit, so batches close on bytes too.
// A var only so tests can shrink it.
var maxBatchBytes int64 = 32 << 20

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

	// bytes admitted but not yet written: raw payloads until parsed, then the
	// parsed logs until their batch lands. Releasing at parse time left every
	// later stage unbounded.
	bufferedBytes atomic.Int64

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
			clampLog(&l)
			p.bufferedBytes.Add(logBytes(&l))
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
	var bufBytes int64
	full := func() bool { return len(buf) >= batchSize || bufBytes >= maxBatchBytes }
	// settle drops the first n rows from buf, written or abandoned
	settle := func(n int) {
		for i := range n {
			sz := logBytes(&buf[i])
			bufBytes -= sz
			p.bufferedBytes.Add(-sz)
		}
		m := copy(buf, buf[n:])
		clear(buf[m:]) // let go of the written strings now, not at the next overwrite
		buf = buf[:m]
	}
	timer := time.NewTimer(batchTimeout)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false

	tryFlush := func() {
		if len(buf) == 0 {
			return
		}
		n, err := len(buf), p.flushBatch(buf)
		switch {
		case db.IsPermanent(err):
			n, err = p.salvage(buf)
		case err != nil:
			n = 0
		}
		settle(n)
		if err != nil {
			log.Error().Err(err).Int("rows", len(buf)).Msg("batch write failed, will retry")
		}
	}
	drain := func() {
		tryFlush()
		if len(buf) > 0 {
			log.Error().Int("rows", len(buf)).Msg("dropping unflushed rows at shutdown")
		}
	}

	for {
		if full() {
			tryFlush()
			if full() {
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
			bufBytes += logBytes(&l)
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
			bufBytes += logBytes(&l)
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

// logBytes is what one log binds into its INSERT (raw, message and parsed are
// separate copies there even when they share a string here).
func logBytes(l *common.StandardLog) int64 {
	n := len(l.Raw)
	if l.Message != nil {
		n += len(*l.Message)
	}
	if l.Parsed != nil {
		n += len(*l.Parsed)
	}
	return int64(n)
}

// clampLog keeps one log under maxLogRowBytes, giving up the derived columns
// first: raw already contains what parsed and message were extracted from.
func clampLog(l *common.StandardLog) {
	if logBytes(l) <= maxLogRowBytes {
		return
	}
	l.Parsed = nil
	if logBytes(l) <= maxLogRowBytes {
		return
	}
	l.Message = nil
	if len(l.Raw) > maxLogRowBytes {
		cut := maxLogRowBytes
		for cut > 0 && !utf8.RuneStart(l.Raw[cut]) {
			cut--
		}
		l.Raw = l.Raw[:cut]
	}
}

// salvage re-writes a batch row by row after a permanent error, dropping only
// the rows SQLite will never accept: retrying the whole batch would block every
// later log behind it forever. Returns how many leading rows are settled.
func (p *Pipeline) salvage(buf []common.StandardLog) (int, error) {
	for i := range buf {
		err := p.flushBatch(buf[i : i+1])
		switch {
		case err == nil:
		case db.IsPermanent(err):
			log.Error().Err(err).Str("service", buf[i].Service).Int("bytes", len(buf[i].Raw)).
				Msg("dropping log row the database rejects")
		default:
			return i, err
		}
	}
	return len(buf), nil
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
