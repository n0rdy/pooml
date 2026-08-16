// Package metrics is the metrics ingestion path: the OTLP push handler and
// the Prometheus scrape loop both feed row batches into a single batch
// writer. Much simpler than logs ingestion by design: no format detection,
// no broadcaster, no FTS. See CONTEXT.md > Metrics.
package metrics

import (
	"database/sql"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	TypeCounter = 0
	TypeGauge   = 1
)

// Producers push whole batches (one per OTLP request / scrape), so the channel
// counts batches, not rows. Flush cadence is relaxed vs logs (1s, not 100ms):
// nothing streams metrics live, dashboards poll.
const (
	batchChanSize = 100
	batchSize     = 500
	flushTimeout  = 1 * time.Second
)

const insertPrefix = "INSERT INTO metrics(timestamp,name,type,value,service,host,labels) VALUES "

type Row struct {
	Timestamp int64 // ms
	Name      string
	Type      int
	Value     float64
	Service   string
	Host      string
	Labels    string // JSON object; empty stores NULL
}

type Pipeline struct {
	db       *sql.DB
	batchCh  chan []Row
	writerWG sync.WaitGroup

	// guards TryPush against send-on-closed-channel when a shutdown deadline
	// expires mid-request; RLock held across the send (see ingestion.Pipeline)
	closeMu sync.RWMutex
	closed  bool
}

func NewPipeline(metricsDB *sql.DB) *Pipeline {
	return &Pipeline{
		db:      metricsDB,
		batchCh: make(chan []Row, batchChanSize),
	}
}

func (p *Pipeline) Start() {
	p.writerWG.Add(1)
	go p.writer()
}

// TryPush enqueues a batch without blocking. False means saturation: the OTLP
// handler returns 429 (the exporter retries with backoff), the scrape loop
// drops the scrape and lets the next interval retry.
func (p *Pipeline) TryPush(rows []Row) bool {
	if len(rows) == 0 {
		return true
	}
	p.closeMu.RLock()
	defer p.closeMu.RUnlock()
	if p.closed {
		return false
	}
	select {
	case p.batchCh <- rows:
		return true
	default:
		return false
	}
}

// Shutdown closes the channel and waits for the final flush. Callers must
// guarantee no TryPush calls after this starts (HTTP servers and the scrape
// loop are already down in main's shutdown sequence).
func (p *Pipeline) Shutdown() {
	p.closeMu.Lock()
	p.closed = true
	p.closeMu.Unlock()
	close(p.batchCh)
	p.writerWG.Wait()
}

func (p *Pipeline) writer() {
	defer p.writerWG.Done()

	buf := make([]Row, 0, batchSize)
	ticker := time.NewTicker(flushTimeout)
	defer ticker.Stop()

	tryFlush := func() {
		for len(buf) > 0 {
			n := min(len(buf), batchSize)
			if err := p.flush(buf[:n]); err != nil {
				log.Error().Err(err).Int("rows", len(buf)).Msg("metrics batch write failed, will retry")
				return
			}
			buf = slices.Delete(buf, 0, n)
		}
	}

	for {
		if len(buf) >= batchSize {
			tryFlush()
			if len(buf) >= batchSize {
				// still failing: back off without pulling more, so the channel
				// fills and producers see saturation
				time.Sleep(flushTimeout)
			}
			continue
		}

		select {
		case rows, ok := <-p.batchCh:
			if !ok {
				tryFlush()
				if len(buf) > 0 {
					log.Error().Int("rows", len(buf)).Msg("dropping unflushed metric rows at shutdown")
				}
				return
			}
			buf = append(buf, rows...)
		case <-ticker.C:
			tryFlush()
		}
	}
}

func (p *Pipeline) flush(rows []Row) error {
	// one multi-row INSERT = one implicit transaction
	q := insertPrefix + strings.TrimSuffix(strings.Repeat("(?,?,?,?,?,?,?),", len(rows)), ",")
	args := make([]any, 0, len(rows)*7)
	for i := range rows {
		r := &rows[i]
		var labels any
		if r.Labels != "" {
			labels = r.Labels
		}
		args = append(args, r.Timestamp, r.Name, r.Type, r.Value, r.Service, r.Host, labels)
	}
	_, err := p.db.Exec(q, args...)
	return err
}
