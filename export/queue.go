package export

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// Queue writes objects to a Sink in the background: bounded, in order,
// each object retried with backoff. An object the queue had no room for
// is dropped and counted; one that still fails after every retry is
// counted too. OnDrop and OnFail, when set, are told (to alert).
type Queue struct {
	sink     Sink
	ch       chan job
	attempts int
	backoff  func(i int) time.Duration
	sleep    func(ctx context.Context, d time.Duration) error

	Dropped, Failed, Written atomic.Int64
	// OnWritten, OnDrop and OnFail are called from the queue's goroutine
	// (OnDrop from the caller's).
	OnWritten func(key string)
	OnDrop    func(key string)
	OnFail    func(key string, err error)

	ctx    context.Context
	cancel context.CancelFunc
	drain  chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
}

type job struct {
	key  string
	body []byte
	o    PutOptions
}

// NewQueue starts a queue of size objects writing to sink.
func NewQueue(sink Sink, size int) *Queue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{sink: sink, ch: make(chan job, size), attempts: 8, ctx: ctx, cancel: cancel, drain: make(chan struct{}),
		backoff: func(i int) time.Duration {
			d := time.Second << min(i, 8) // 1 s doubling to about 4 min
			return d + time.Duration(rand.Int64N(int64(d)/4+1))
		},
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
	q.wg.Add(1)
	go q.run()
	return q
}

// Sink is the queue's destination.
func (q *Queue) Sink() Sink { return q.sink }

// Enqueue adds an object without blocking; it reports false when the
// queue was full and the object dropped.
func (q *Queue) Enqueue(key string, body []byte, o PutOptions) bool {
	select {
	case q.ch <- job{key, body, o}:
		return true
	default:
		q.Dropped.Add(1)
		if q.OnDrop != nil {
			q.OnDrop(key)
		}
		return false
	}
}

func (q *Queue) run() {
	defer q.wg.Done()
	for {
		select {
		case j := <-q.ch:
			q.write(j)
		case <-q.drain:
			// Write what is left, then stop (ctx bounds it).
			for {
				select {
				case j := <-q.ch:
					q.write(j)
				default:
					return
				}
			}
		case <-q.ctx.Done():
			return
		}
	}
}

func (q *Queue) write(j job) {
	var err error
	for i := 0; i < q.attempts; i++ {
		if i > 0 {
			if q.sleep(q.ctx, q.backoff(i-1)) != nil {
				break
			}
		}
		ctx, cancel := context.WithTimeout(q.ctx, time.Minute)
		err = q.sink.Put(ctx, j.key, j.body, j.o)
		cancel()
		if err == nil {
			q.Written.Add(1)
			if q.OnWritten != nil {
				q.OnWritten(j.key)
			}
			return
		}
	}
	q.Failed.Add(1)
	if q.OnFail != nil {
		q.OnFail(j.key, err)
	}
}

// Close writes what is queued, waiting up to ctx, then stops.
func (q *Queue) Close(ctx context.Context) {
	q.once.Do(func() {
		close(q.drain)
		done := make(chan struct{})
		go func() { q.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			q.cancel()
			<-done
		}
		q.cancel()
	})
}
