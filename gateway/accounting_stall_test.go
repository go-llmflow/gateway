package gateway

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/spool"
	"github.com/go-llmflow/gateway/store"
)

// hangStore is a store whose InsertRecords does not return until released or
// its context ends: PostgreSQL restarting, or slow (chaos: ha-kill-during-pg-restart).
type hangStore struct {
	*store.Memory
	release chan struct{}
}

func (h *hangStore) InsertRecords(ctx context.Context, recs []store.Record) error {
	select {
	case <-h.release:
		return h.Memory.InsertRecords(ctx, recs)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reproducer (gateway v0.38.0, gateway/accounting.go:125-143 and :221-257):
// the recorder has ONE goroutine, and flush calls drain, which calls insert
// with a 10 s timeout. While the store hangs, that goroutine is inside insert,
// so records that arrive meanwhile wait in the in-memory queue and are not
// spooled. A SIGKILL then loses all of them: the documented bound is "at most
// about 1 s of traffic" (README, durable accounting); here it is the length of
// the store's stall (up to 10 s per attempt, repeated at each tick).
//
// The test reads the spool file the way the next process would after a crash,
// and wants every record that was recorded before the "crash" to be on disk.
func TestSpoolHoldsEveryRecordWhileTheStoreHangs(t *testing.T) {
	dir := t.TempDir()
	sp, err := spool.Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	hs := &hangStore{Memory: store.NewMemory(), release: make(chan struct{})}
	defer close(hs.release)
	r := newRecorder(hs, 100, 10, nil)
	r.interval = 50 * time.Millisecond
	r.withSpool(sp, dir, 1<<20)
	r.start()

	r.record(store.Record{ID: "r1"})
	time.Sleep(300 * time.Millisecond) // r1 is spooled, the loop is now stuck inserting it
	r.record(store.Record{ID: "r2"})   // answered while the store hangs
	r.record(store.Record{ID: "r3"})
	time.Sleep(300 * time.Millisecond) // several ticks later; the process is SIGKILLed here

	f, err := os.Open(sp.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, _, err := spool.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("a crash now loses %d of 3 answered requests' records (on disk: %d); records wait in memory while the store hangs", 3-len(recs), len(recs))
	}
}
