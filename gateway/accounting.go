package gateway

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/gateway/spool"
	"github.com/go-llmflow/gateway/store"
)

// recorder writes usage records asynchronously through a bounded queue.
// A full queue drops the record (and counts it) rather than blocking the
// request path; budgets still see the cost through budgeter.add.
//
// With a spool (store kind postgres), every batch is appended to the
// process's spool file and fsync'd before it is inserted; a batch the store
// does not take stays pending, in order, and is retried at every tick until
// the store is back. Spool files left by a process that died are adopted
// and replayed. Inserts are keyed by the record ID, so a batch written
// just before a crash and replayed after it is billed once. Without a
// spool (memory store), a batch is tried twice, then counted as failed.
type recorder struct {
	st       store.Store
	mu       sync.RWMutex // guards closed against record racing close
	closed   bool
	queue    chan store.Record
	batch    int
	dropped  atomic.Int64 // queue full
	failed   atomic.Int64 // neither stored nor spooled: lost
	written  atomic.Int64
	spooled  atomic.Int64 // pending in the spool
	replayed atomic.Int64 // adopted from a dead process's spool and stored
	wg       sync.WaitGroup
	onErr    func(error)
	// onLost is told how many records were lost because the spool was
	// full or unwritable, or that a spool file was corrupt (to alert).
	onLost   func(n int, err error)
	interval time.Duration
	// done is told about every record once it is stored, failed or
	// dropped (the budgeter moves its cost from pending to written).
	done func([]store.Record)
	// onAdopted is told the id of a dead process whose spool was replayed
	// (its budget grants are released).
	onAdopted func(id string) error

	spool      *spool.Spool
	spoolDir   string
	spoolMax   int64
	pending    []store.Record // spooled, not yet stored, oldest first
	nextAdopt  time.Time
	adoptEvery time.Duration
}

func newRecorder(st store.Store, size, batch int, onErr func(error)) *recorder {
	return &recorder{st: st, queue: make(chan store.Record, size), batch: batch, onErr: onErr, interval: time.Second,
		adoptEvery: 10 * time.Second}
}

// withSpool makes the recorder durable through sp (in dir).
func (r *recorder) withSpool(sp *spool.Spool, dir string, max int64) {
	r.spool, r.spoolDir, r.spoolMax = sp, dir, max
}

func (r *recorder) record(rec store.Record) {
	if rec.ID == "" {
		rec.ID = requestID()
	}
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
	if r.spool != nil {
		r.adopt() // what a dead process left, before anything new
	}
	r.wg.Add(1)
	go r.run()
}

func (r *recorder) insert(recs []store.Record) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return r.st.InsertRecords(ctx, recs)
}

// flush writes one batch of new records.
func (r *recorder) flush(buf []store.Record) {
	if len(buf) == 0 {
		return
	}
	if r.spool == nil {
		err := r.insert(buf)
		if err != nil {
			// One retry, then the batch is counted as failed.
			err = r.insert(buf)
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
		return
	}
	batch := append([]store.Record(nil), buf...)
	if err := r.spool.Append(batch); err != nil {
		// Not durable: still try the store directly, once.
		if ierr := r.insert(batch); ierr == nil {
			r.written.Add(int64(len(batch)))
		} else {
			r.failed.Add(int64(len(batch)))
			if r.onLost != nil {
				r.onLost(len(batch), errors.Join(err, ierr))
			}
		}
		if r.done != nil {
			r.done(batch)
		}
		r.drain()
		return
	}
	r.pending = append(r.pending, batch...)
	r.spooled.Store(int64(len(r.pending)))
	r.drain()
}

// drain inserts the pending records, oldest first, until the store
// refuses; when nothing is pending the spool is emptied.
func (r *recorder) drain() {
	for len(r.pending) > 0 {
		n := min(len(r.pending), r.batch)
		chunk := r.pending[:n]
		if err := r.insert(chunk); err != nil {
			if r.onErr != nil {
				r.onErr(err)
			}
			break
		}
		r.written.Add(int64(n))
		if r.done != nil {
			r.done(chunk)
		}
		r.pending = append(r.pending[:0:0], r.pending[n:]...)
	}
	r.spooled.Store(int64(len(r.pending)))
	if len(r.pending) == 0 {
		if err := r.spool.Reset(); err != nil && r.onErr != nil {
			r.onErr(err)
		}
	}
}

// adopt replays the spool files of processes that are gone.
func (r *recorder) adopt() {
	r.nextAdopt = time.Now().Add(r.adoptEvery)
	orphans, err := spool.Orphans(r.spoolDir, r.spool.Path(), r.spoolMax)
	if err != nil {
		if r.onErr != nil {
			r.onErr(err)
		}
		return
	}
	for _, o := range orphans {
		ok := true
		for i := 0; i < len(o.Records); i += r.batch {
			chunk := o.Records[i:min(i+r.batch, len(o.Records))]
			if err := r.insert(chunk); err != nil {
				ok = false
				if r.onErr != nil {
					r.onErr(err)
				}
				break
			}
		}
		if !ok {
			o.Release() // the store is down: next tick
			continue
		}
		// Every record the dead process spooled is now settled: its grants
		// in the budget ledger hold nothing in flight any more (what it never
		// spooled was never charged). Released before the file goes, so a
		// failure leaves both for the next tick.
		if r.onAdopted != nil {
			if err := r.onAdopted(o.ID()); err != nil {
				o.Release()
				if r.onErr != nil {
					r.onErr(err)
				}
				continue
			}
		}
		r.replayed.Add(int64(len(o.Records)))
		if o.Tail == spool.Corrupt && r.onLost != nil {
			r.onLost(0, errors.New("spool file "+o.Path+" is corrupt after its last readable record; kept as .corrupt"))
		}
		if err := o.Done(); err != nil && r.onErr != nil {
			r.onErr(err)
		}
	}
}

func (r *recorder) run() {
	defer r.wg.Done()
	buf := make([]store.Record, 0, r.batch)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	flush := func() {
		r.flush(buf)
		buf = buf[:0]
	}
	for {
		select {
		case rec, ok := <-r.queue:
			if !ok {
				flush()
				if r.spool != nil {
					r.drain()
					_ = r.spool.Close() // kept on disk when records remain
				}
				return
			}
			buf = append(buf, rec)
			if len(buf) >= r.batch {
				flush()
			}
		case <-t.C:
			flush()
			if r.spool != nil {
				if len(r.pending) > 0 {
					r.drain()
				}
				if time.Now().After(r.nextAdopt) {
					r.adopt()
				}
			}
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
