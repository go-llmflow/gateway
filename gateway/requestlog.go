package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/export"
	"github.com/go-llmflow/gateway/store"
)

// Per-team request logs. A team named in a logging block has every
// inference request written to that block's export, in gzipped JSON-lines
// batches: the spend metadata always, the prompt and the completion only
// when the block says content = true. A team in no block has no request
// log at all — spend metadata in the store, nothing else — which is the
// default for everybody. Content is never written to the gateway's own log.

// maxBatchBytes flushes a batch whose compressed size reaches it.
const maxBatchBytes = 8 << 20

type requestLogger struct {
	cfg config.Logging
	q   *export.Queue
	now func() time.Time

	mu      sync.Mutex
	buf     bytes.Buffer
	zw      *gzip.Writer
	records int
	seq     atomic.Int64
}

// requestLogRecord is one line of a request log.
type requestLogRecord struct {
	store.Record
	RequestID string       `json:"request_id"`
	Content   *contentPart `json:"content,omitempty"`
}

type contentPart struct {
	Request           json.RawMessage `json:"request,omitempty"`
	RequestTruncated  bool            `json:"request_truncated,omitempty"`
	Response          string          `json:"response,omitempty"`
	ResponseTruncated bool            `json:"response_truncated,omitempty"`
}

func newRequestLogger(cfg config.Logging, q *export.Queue, now func() time.Time) *requestLogger {
	l := &requestLogger{cfg: cfg, q: q, now: now}
	l.zw = gzip.NewWriter(&l.buf)
	return l
}

// add appends a record; it flushes when the batch is full.
func (l *requestLogger) add(r requestLogRecord) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.zw.Write(append(b, '\n'))
	l.records++
	// A batch is bounded in bytes too: content of up to 16 MiB per
	// record times batch_records must not pile up in memory.
	if l.records >= l.cfg.BatchRecords || l.buf.Len() >= maxBatchBytes {
		l.flushLocked()
	}
}

// flushLocked hands the batch to the export.
func (l *requestLogger) flushLocked() {
	if l.records == 0 {
		return
	}
	_ = l.zw.Close()
	body := append([]byte(nil), l.buf.Bytes()...)
	t := l.now().UTC()
	key := fmt.Sprintf("requests/%s/%s/%d-%06d.jsonl.gz", l.cfg.Name, t.Format("2006/01/02"), t.UnixNano(), l.seq.Add(1))
	l.q.Enqueue(key, body, export.PutOptions{ContentType: "application/x-ndjson", ContentEncoding: "gzip", Retain: l.cfg.Lock})
	l.buf.Reset()
	l.zw.Reset(&l.buf)
	l.records = 0
}

func (l *requestLogger) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
}

// loop flushes every batch_interval, and once more at the end.
func (l *requestLogger) loop(ctx context.Context) {
	t := time.NewTicker(l.cfg.BatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.flush()
			return
		case <-t.C:
			l.flush()
		}
	}
}

// uniqueRequestLogs lists each request log once (several teams share one).
func (s *Server) uniqueRequestLogs() []*requestLogger {
	seen := map[*requestLogger]bool{}
	var out []*requestLogger
	for _, lc := range s.cfg.Logging {
		for _, t := range lc.Teams {
			if l := s.requestLogs[t]; l != nil && !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	return out
}

// logsContent says whether a team's request log keeps content.
func (s *Server) logsContent(team string) (int, bool) {
	l := s.requestLogs[team]
	if l == nil || !l.cfg.Content {
		return 0, false
	}
	return l.cfg.MaxContentBytes, true
}

// cutBytes cuts b at max bytes.
func cutBytes(b []byte, max int) ([]byte, bool) {
	if len(b) <= max {
		return b, false
	}
	return b[:max], true
}

// captureWriter keeps up to max bytes of what is written through it (a
// streamed answer), for a content log.
type captureWriter struct {
	http.ResponseWriter
	max       int
	buf       bytes.Buffer
	truncated bool
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
		if len(p) > room {
			c.truncated = true
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return c.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach Flush.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// logRequest writes the request log record of a finished request, if its
// team has a request log.
func (s *Server) logRequest(rec store.Record, rid string, reqBody []byte, respBody []byte, respTruncated bool) {
	l := s.requestLogs[rec.Team]
	if l == nil {
		return
	}
	r := requestLogRecord{Record: rec, RequestID: rid}
	if l.cfg.Content {
		c := &contentPart{}
		body, cut := cutBytes(reqBody, l.cfg.MaxContentBytes)
		if json.Valid(body) {
			c.Request = body
		} else {
			// A cut JSON body is kept as a string.
			s, _ := json.Marshal(string(body))
			c.Request = s
		}
		c.RequestTruncated = cut
		resp, rcut := cutBytes(respBody, l.cfg.MaxContentBytes)
		c.Response, c.ResponseTruncated = string(resp), rcut || respTruncated
		r.Content = c
	}
	l.add(r)
}
