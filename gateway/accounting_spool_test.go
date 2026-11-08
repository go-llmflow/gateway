package gateway

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/spool"
	"github.com/go-llmflow/gateway/store"
)

// crashed reads a recorder's spool file as the next process would after a
// SIGKILL, and replays it into st the way adopt does: in chunks, keyed by ID.
func replayInto(t *testing.T, sp *spool.Spool, st store.Store) int {
	t.Helper()
	f, err := os.Open(sp.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, _, err := spool.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRecords(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	return len(recs)
}

func stored(t *testing.T, m *store.Memory) map[string]int {
	t.Helper()
	got := map[string]int{}
	for _, r := range m.Records() {
		got[r.ID]++
	}
	return got
}

// Exactly once across a crash at each step of spool-then-store (#165): after
// the spool and before the insert; after the insert and before the spool is
// emptied (the file still holds records the store has). The replay is keyed by
// record ID, so the second insert changes nothing.
func TestCrashAtEachStepBillsEveryRecordOnce(t *testing.T) {
	for _, step := range []string{"spooled, not inserted", "inserted, spool not emptied"} {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			sp, err := spool.Open(dir, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			mem := store.NewMemory()
			hs := &hangStore{Memory: mem, release: make(chan struct{})}
			r := newRecorder(hs, 100, 10, nil)
			r.interval = 20 * time.Millisecond
			r.withSpool(sp, dir, 1<<20)
			r.start()
			for _, id := range []string{"a", "b", "c"} {
				r.record(store.Record{ID: id})
			}
			time.Sleep(200 * time.Millisecond) // on disk; the store is hung
			if step == "inserted, spool not emptied" {
				// The store took them, but the process died before Reset.
				if err := mem.InsertRecords(context.Background(), []store.Record{{ID: "a"}, {ID: "b"}, {ID: "c"}}); err != nil {
					t.Fatal(err)
				}
			}
			if n := replayInto(t, sp, mem); n != 3 {
				t.Fatalf("%d records on disk, want 3", n)
			}
			for id, n := range stored(t, mem) {
				if n != 1 {
					t.Errorf("record %s stored %d times", id, n)
				}
			}
			if got := stored(t, mem); len(got) != 3 {
				t.Errorf("%d distinct records stored, want 3: %v", len(got), got)
			}
			close(hs.release)
			r.close(context.Background())
		})
	}
}

// A full spool: the records are held in memory only and reach the store when it
// is back, the recorder says it is degraded, and a request under a budget is
// refused (fail closed) while it is.
func TestAFullSpoolRefusesBudgetedRequestsAndKeepsTheRecords(t *testing.T) {
	dir := t.TempDir()
	sp, err := spool.Open(dir, 600) // room for one record, not two
	if err != nil {
		t.Fatal(err)
	}
	mem := store.NewMemory()
	hs := &hangStore{Memory: mem, release: make(chan struct{})}
	r := newRecorder(hs, 100, 10, nil)
	r.interval = 20 * time.Millisecond
	lost := make(chan int, 4)
	r.onLost = func(n int, _ error) { lost <- n }
	r.withSpool(sp, dir, 200) // room for about one record
	r.start()
	for _, id := range []string{"a", "b", "c", "d"} {
		r.record(store.Record{ID: id})
		time.Sleep(60 * time.Millisecond)
	}
	if !r.spoolUnavailable() {
		t.Fatal("the recorder does not say its spool is full")
	}
	select {
	case n := <-lost:
		if n != 0 {
			t.Errorf("%d records lost with room in memory", n)
		}
	case <-time.After(time.Second):
		t.Fatal("no alert that the spool is full")
	}
	close(hs.release) // the store is back
	r.close(context.Background())
	if got := stored(t, mem); len(got) != 4 {
		t.Errorf("%d of 4 records reached the store from memory: %v", len(got), got)
	}
}

func TestABudgetedRequestIsRefusedWhileTheSpoolIsFull(t *testing.T) {
	e := newEnv(t, testConfig(newFakeVLLM(t).URL()))
	_, key := e.newKey("etabA")
	resp := e.do("PUT", "/admin/budgets", master, `{"kind":"team","name":"etabA","period":"day","max_cost":100}`)
	readAll(t, resp)
	e.srv.rec.spool = &spool.Spool{} // a spool is configured...
	e.srv.rec.degraded.Store(true)   // ...and it refuses records
	defer func() { e.srv.rec.spool = nil }()
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	if out := readAll(t, resp); resp.StatusCode != 503 || !strings.Contains(out, "accounting spool") {
		t.Fatalf("a budgeted request with a full spool: %d %s", resp.StatusCode, out)
	}
	e.srv.rec.degraded.Store(false)
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	if readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("after the spool recovered: %d", resp.StatusCode)
	}
}

// (b) A batch that fits does not clear "degraded" while older records are still
// only in memory (audit 2026-11-08): a big batch the spool refused, then a small
// one it takes.
func TestDegradedStaysWhileOlderRecordsAreOnlyInMemory(t *testing.T) {
	small := store.Record{ID: "small-1"}
	big := store.Record{ID: "big", Model: strings.Repeat("m", 5000)}
	enc := func(rs ...store.Record) int {
		b, err := spool.Encode(nil, rs)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	dir := t.TempDir()
	sp, err := spool.Open(dir, int64(enc(small)*2+10)) // two small records, no big one
	if err != nil {
		t.Fatal(err)
	}
	hs := &hangStore{Memory: store.NewMemory(), release: make(chan struct{})}
	r := newRecorder(hs, 100, 1, nil)
	r.interval = 10 * time.Millisecond
	r.onLost = func(int, error) {}
	r.withSpool(sp, dir, int64(enc(small)*2+10))
	r.start()
	r.record(big)
	time.Sleep(100 * time.Millisecond)
	if !r.spoolUnavailable() {
		t.Fatal("not degraded after a refused batch")
	}
	r.record(store.Record{ID: "small-2"}) // fits in the file
	time.Sleep(100 * time.Millisecond)
	if !r.spoolUnavailable() {
		t.Error("a batch that fit cleared degraded while the big record is still only in memory")
	}
	close(hs.release)
	r.close(context.Background())
}

// (a) One organisation's unstored records cannot fill the spool for everybody:
// past half the file, the organisation holding most of the pending records is
// refused, the others are not.
func TestOneOrganisationCannotFillTheSpoolForTheOthers(t *testing.T) {
	rec := func(id, org string) store.Record { return store.Record{ID: id, Org: org} }
	b, _ := spool.Encode(nil, []store.Record{rec("x", "A")})
	max := int64(len(b)*5 + 10)
	dir := t.TempDir()
	sp, err := spool.Open(dir, max)
	if err != nil {
		t.Fatal(err)
	}
	hs := &hangStore{Memory: store.NewMemory(), release: make(chan struct{})}
	r := newRecorder(hs, 100, 1, nil)
	r.interval = 10 * time.Millisecond
	r.withSpool(sp, dir, max)
	r.start()
	for _, id := range []string{"a1", "a2", "a3"} {
		r.record(rec(id, "A"))
	}
	time.Sleep(200 * time.Millisecond)
	if !r.orgHoldsTooMuch("A") {
		t.Error("the organisation that fills the spool is not refused")
	}
	if r.orgHoldsTooMuch("B") {
		t.Error("another organisation is refused for A's volume")
	}
	r.record(rec("b1", "B"))
	time.Sleep(100 * time.Millisecond)
	if r.spoolUnavailable() {
		t.Error("B's record did not fit in the headroom A left")
	}
	close(hs.release)
	r.close(context.Background())
}

// (c) With the memory-only records at their bound nothing more can be recorded,
// so NO request is admitted, budgeted or not.
func TestEveryRequestIsRefusedWhenTheMemoryOnlyRecordsAreAtTheirBound(t *testing.T) {
	e := newEnv(t, testConfig(newFakeVLLM(t).URL()))
	_, key := e.newKey("etabA")
	e.srv.rec.spool = &spool.Spool{}
	e.srv.rec.pmu.Lock()
	e.srv.rec.volatile = maxVolatile
	e.srv.rec.pmu.Unlock()
	defer func() {
		e.srv.rec.pmu.Lock()
		e.srv.rec.volatile = 0
		e.srv.rec.pmu.Unlock()
		e.srv.rec.spool = nil
	}()
	resp := e.do("POST", "/v1/chat/completions", key, chat) // no budget applies
	if out := readAll(t, resp); resp.StatusCode != 503 || !strings.Contains(out, "accounting spool") {
		t.Fatalf("an unbudgeted request with the memory-only records at their bound: %d %s", resp.StatusCode, out)
	}
}
