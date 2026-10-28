package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Through the HTTP API: a budget of 0.3 is created while a request of its
// scope (worst case 0.208, cost 0.164) runs at vLLM. It reserves that
// request from its creation, so a second request (worst case 0.208) does
// not fit, and the budget ends at 0.164. While another registered process
// has not reported, nothing is granted at all, and the refusal says so.
//
// Planted control: created as v0.17.0 did (active at once), the budget
// admits the second request, and the two cost 0.328 > 0.3.
func TestBudgetArmsOverARunningRequest(t *testing.T) {
	team := store.BudgetKey{Kind: store.ScopeTeam, Name: "etabA"}
	body := `{"model":"llama","max_tokens":20,"messages":[]}`
	for _, control := range []bool{false, true} {
		up := newFakeVLLM(t)
		up.gate = make(chan struct{})
		e := newEnv(t, testConfig(up.URL()))
		e.st.NoArming = control
		key := e.createKey(`{"team":"etabA"}`)
		first := make(chan int, 1)
		go func() { first <- e.chatWith(key, body) }()
		var once sync.Once
		release := func() { once.Do(func() { close(up.gate) }) }
		t.Cleanup(release) // a failure must not leave the gateway waiting on vLLM
		waitHits := func(n int) {
			deadline := time.Now().Add(30 * time.Second)
			for up.count() < n {
				if time.Now().After(deadline) {
					release()
					t.Fatalf("%d requests reached vLLM, want %d", up.count(), n)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		waitHits(1)
		ghost := store.Holder{Replica: "other", Incarnation: "o1"}
		if err := e.st.RegisterHolder(context.Background(), ghost); err != nil {
			t.Fatal(err)
		}
		e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":0.3}`)
		second := make(chan string, 1)
		go func() {
			resp := e.do("POST", "/v1/chat/completions", key, body)
			second <- resp.Status + " " + readAll(t, resp)
		}()
		if control {
			waitHits(2) // admitted: it reached vLLM
			release()
			<-first
			<-second
			e.srv.Close(context.Background())
			if st, _ := e.st.Ledger(context.Background(), team, store.Day.Start(time.Now())); st.Spent <= 300_000_000 {
				t.Fatalf("control: spent %d ≤ the limit: the test cannot see this overrun", st.Spent)
			}
			continue
		}
		if out := <-second; !strings.HasPrefix(out, "429") || !strings.Contains(out, "arming") || !strings.Contains(out, "team etabA") {
			t.Fatalf("another process has not reported: %s", out)
		}
		if _, err := e.st.ReportArming(context.Background(), ghost, team, 0); err != nil {
			t.Fatal(err)
		}
		e.srv.escrow.forgetRoom()
		resp := e.do("POST", "/v1/chat/completions", key, body)
		if out := readAll(t, resp); resp.StatusCode != 429 || !strings.Contains(out, "0.092000000 gpu_hours left") {
			st, _ := e.st.Ledger(context.Background(), team, store.Day.Start(time.Now()))
			release()
			t.Fatalf("beside the running request (0.3 - 0.208 reserved): %d %s (ledger %+v)", resp.StatusCode, out, st)
		}
		release()
		if code := <-first; code != 200 {
			t.Fatalf("the running request: %d", code)
		}
		e.waitRecords(1)
		e.srv.escrow.forgetRoom()
		if code := e.chatWith(key, body); code != 429 { // 0.3 - 0.164 = 0.136 < 0.208
			t.Fatalf("after it settled: %d", code)
		}
		e.srv.Close(context.Background())
		// The running request's cost landed in the budget.
		if st, _ := e.st.Ledger(context.Background(), team, store.Day.Start(time.Now())); st.Spent != 164_000_000 {
			t.Fatalf("ledger %+v", st)
		}
	}
}
