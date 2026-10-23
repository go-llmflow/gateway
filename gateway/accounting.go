package gateway

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// recorder writes usage records asynchronously through a bounded queue.
// A full queue drops the record (and counts it) rather than blocking the
// request path; budgets still see the cost through budgeter.add.
type recorder struct {
	st       store.Store
	mu       sync.RWMutex // guards closed against record racing close
	closed   bool
	queue    chan store.Record
	batch    int
	dropped  atomic.Int64
	failed   atomic.Int64
	written  atomic.Int64
	wg       sync.WaitGroup
	onErr    func(error)
	interval time.Duration
	// done is told about every record once it is stored, failed or
	// dropped (the budgeter moves its cost from pending to written).
	done func([]store.Record)
}

func newRecorder(st store.Store, size, batch int, onErr func(error)) *recorder {
	return &recorder{st: st, queue: make(chan store.Record, size), batch: batch, onErr: onErr, interval: time.Second}
}

func (r *recorder) record(rec store.Record) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.closed {
		select {
		case r.queue <- rec:
			return
		default:
		}
	}
	r.dropped.Add(1)
	if r.done != nil {
		r.done([]store.Record{rec})
	}
}

func (r *recorder) start() {
	r.wg.Add(1)
	go r.run()
}

func (r *recorder) run() {
	defer r.wg.Done()
	buf := make([]store.Record, 0, r.batch)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	flush := func() {
		if len(buf) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := r.st.InsertRecords(ctx, buf)
		cancel()
		if err != nil {
			// One retry, then the batch is counted as failed: holding it
			// longer would grow memory without bound while the store is
			// down.
			ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
			err = r.st.InsertRecords(ctx, buf)
			cancel()
		}
		if err != nil {
			r.failed.Add(int64(len(buf)))
			if r.onErr != nil {
				r.onErr(err)
			}
		} else {
			r.written.Add(int64(len(buf)))
		}
		if r.done != nil {
			r.done(buf)
		}
		buf = buf[:0]
	}
	for {
		select {
		case rec, ok := <-r.queue:
			if !ok {
				flush()
				return
			}
			buf = append(buf, rec)
			if len(buf) >= r.batch {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// close drains the queue and waits for the writer, at most until ctx ends.
func (r *recorder) close(ctx context.Context) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.queue)
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
