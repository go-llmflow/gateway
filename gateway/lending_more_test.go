package gateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// A pool's C is read from its history: a point before the period applies from
// its start, and a configured capacity of 0 (a Slurm pool) is not the C.
func TestLendingUsesTheCapacityHistory(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Pools[0].Capacity = 0 // as a pool fed by Slurm jobs
	e := newEnv(t, cfg)
	ctx := context.Background()
	day := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	if err := e.st.RecordCapacity(ctx, store.CapacityPoint{Pool: "b300", At: day.Add(-48 * time.Hour), C: 8}); err != nil {
		t.Fatal(err)
	}
	// C doubles at noon: etabA (0.75) reserves 6 slots, then 12.
	if err := e.st.RecordCapacity(ctx, store.CapacityPoint{Pool: "b300", At: day.Add(12 * time.Hour), C: 16}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.RecordCapacity(ctx, store.CapacityPoint{Pool: "b300", At: day.Add(12 * time.Hour), C: 16}); err != nil { // a second replica's copy
		t.Fatal(err)
	}
	resp := e.do("GET", "/admin/lending?from=2026-10-10&to=2026-10-11", master, "")
	var a struct {
		Orgs []LendingRow `json:"orgs"`
	}
	if err := json.Unmarshal([]byte(readAll(t, resp)), &a); err != nil || len(a.Orgs) != 2 {
		t.Fatalf("%v %+v", err, a)
	}
	// 6 slots for 12 h + 12 slots for 12 h = 216 slot-hours; etabB 2×12 + 4×12 = 72.
	if !near(a.Orgs[0].GuaranteedHours, 216) || !near(a.Orgs[1].GuaranteedHours, 72) {
		t.Fatalf("guaranteed %+v", a.Orgs)
	}
}

// What the gateway applies is what the history says.
func TestNoteCapacity(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	ctx := context.Background()
	e.srv.noteCapacity("b300")
	e.srv.noteCapacity("b300") // unchanged: nothing more
	pts, _ := e.st.CapacityBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if len(pts) != 1 || pts[0].C != 4 || pts[0].Pool != "b300" {
		t.Fatalf("%+v", pts)
	}
	if p, ok := e.srv.pools["b300"].(elasticPool); ok {
		p.setCapacity(10)
	} else {
		t.Fatal("not an elastic pool")
	}
	e.srv.noteCapacity("b300")
	if pts, _ = e.st.CapacityBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); len(pts) != 2 || pts[1].C != 10 {
		t.Fatalf("%+v", pts)
	}
}

// A request records the time it waited for its grant, inside its latency.
func TestRequestRecordsAdmissionWait(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA")
	if code := e.chatWith(key, `{"model":"llama","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`); code != 200 {
		t.Fatalf("chat: %d", code)
	}
	e.waitRecords(1)
	r := e.st.Records()[0]
	if r.AdmissionWaitMS < 0 || r.AdmissionWaitMS > r.LatencyMS {
		t.Fatalf("wait %v ms, latency %v ms", r.AdmissionWaitMS, r.LatencyMS)
	}
}

// Heavy reports are bounded: when every slot is taken the caller is told to
// come back, and a slot freed is a slot usable.
func TestHeavyReportsAreBounded(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	q := "/admin/lending?from=2026-10-01&to=2026-10-02"
	for i := 0; i < heavyReports; i++ {
		heavySem <- struct{}{}
	}
	resp := e.do("GET", q, master, "")
	_ = readAll(t, resp)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("full: %d %v", resp.StatusCode, resp.Header)
	}
	for i := 0; i < heavyReports; i++ {
		<-heavySem
	}
	if resp = e.do("GET", q, master, ""); resp.StatusCode != 200 {
		t.Fatalf("free: %d", resp.StatusCode)
	}
	_ = readAll(t, resp)
}
