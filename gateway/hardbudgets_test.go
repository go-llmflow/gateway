package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Hard budgets (#87), through the HTTP API. Worst cases with the test
// prices and max_model_len 128 (budgets_test.go): W(M) = 0.128 + 0.004·M.

// The client's max_tokens is never lowered: a request whose worst case does
// not fit is refused, naming the room left and the request's maximum cost.
// Without one, the gateway picks the largest that fits and says so.
func TestHardBudgetMaxTokens(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	key := e.createKey(`{"team":"etabA"}`)
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":0.3}`)

	n := up.count()
	resp := e.do("POST", "/v1/chat/completions", key, `{"model":"llama","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	body := readAll(t, resp)
	if resp.StatusCode != 429 || !strings.Contains(body, "insufficient_quota") || !strings.Contains(body, "team etabA") ||
		!strings.Contains(body, "0.300000000 gpu_hours left") || !strings.Contains(body, "up to 0.328000000") || up.count() != n {
		t.Fatalf("a client bound that does not fit: %d %s", resp.StatusCode, body)
	}
	// It names the caller's own scope only.
	for _, other := range []string{"etabB", "org etabA"} {
		if strings.Contains(body, other) {
			t.Fatalf("the refusal names %q: %s", other, body)
		}
	}
	resp = e.do("POST", "/v1/chat/completions", key, `{"model":"llama","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`)
	if readAll(t, resp); resp.StatusCode != 200 || resp.Header.Get("X-Llmflow-Max-Tokens") != "" {
		t.Fatalf("a client bound that fits: %d %q", resp.StatusCode, resp.Header.Get("X-Llmflow-Max-Tokens"))
	}
	if got := up.last().Body; got["max_tokens"] != float64(20) || got["max_completion_tokens"] != float64(20) {
		t.Fatalf("sent %v / %v", got["max_tokens"], got["max_completion_tokens"])
	}
	// 0.3 - 0.164 = 0.136 left: W(16) = 0.192 does not fit, so no bound
	// the gateway could pick; with a larger budget it picks.
	expect(t, "no bound fits", e.chatWith(key, chat), 429)
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":0.5}`)
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	readAll(t, resp)
	// 0.5 - 0.164 = 0.336: M = (0.336 - 0.128) / 0.004 = 52.
	if resp.StatusCode != 200 || resp.Header.Get("X-Llmflow-Max-Tokens") != "52" || up.last().Body["max_tokens"] != float64(52) {
		t.Fatalf("picked bound: %d %q %v", resp.StatusCode, resp.Header.Get("X-Llmflow-Max-Tokens"), up.last().Body["max_tokens"])
	}
}

// A model's max_output_tokens caps the client's bound, and becomes the
// bound of a request that sent none.
func TestMaxOutputTokensCap(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.ModelGroups[0].MaxOutputTokens = 10
	e := newEnv(t, cfg)
	key := e.createKey(`{"team":"etabA"}`)
	resp := e.do("POST", "/v1/chat/completions", key, `{"model":"llama","max_tokens":11,"messages":[]}`)
	if body := readAll(t, resp); resp.StatusCode != 400 || !strings.Contains(body, "max_output_tokens (10)") {
		t.Fatalf("above the cap: %d %s", resp.StatusCode, body)
	}
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	if readAll(t, resp); resp.StatusCode != 200 || resp.Header.Get("X-Llmflow-Max-Tokens") != "10" || up.last().Body["max_tokens"] != float64(10) {
		t.Fatalf("no bound: %d %v", resp.StatusCode, up.last().Body["max_tokens"])
	}
}

// What cannot be bounded is refused under a budget only; a model whose
// context length is unknown is refused under a budget only.
func TestUnboundedRefusedUnderBudget(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	key := e.createKey(`{"team":"etabA"}`)
	tool := `{"model":"llama","input":"x","max_output_tokens":8,"tools":[{"type":"web_search_preview"}]}`
	if code, _ := e.responses(key, tool); code != 200 {
		t.Fatalf("no budget: %d", code)
	}
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":100}`)
	if code, out := e.responses(key, tool); code != 400 || !strings.Contains(out, "cannot be bounded") {
		t.Fatalf("built-in tool under a budget: %d %s", code, out)
	}
	if code, _ := e.responses(key, `{"model":"llama","input":"x","max_output_tokens":8}`); code != 200 {
		t.Fatalf("plain responses under a budget: %d", code)
	}
	for _, u := range e.srv.groups["llama"].members() {
		u.cards.Store(nil) // no upstream reported its context length
	}
	resp := e.do("POST", "/v1/chat/completions", key, chat)
	if body := readAll(t, resp); resp.StatusCode != 503 || !strings.Contains(body, "context length") {
		t.Fatalf("unknown context length: %d %s", resp.StatusCode, body)
	}
}

// A burst: every request admitted together is held at its worst case, so
// the budget is never exceeded however many arrive at once. The planted
// control (the pre-#87 check) admits them all and overshoots.
func TestBurstNeverExceeds(t *testing.T) {
	for _, soft := range []bool{false, true} {
		up := newFakeVLLM(t)
		up.gate = make(chan struct{})
		e := newEnv(t, testConfig(up.URL()))
		e.srv.softBudgets = soft
		key := e.createKey(`{"team":"etabA"}`)
		// Room for 5 worst cases of max_tokens 20 (0.208 each): 1.05.
		e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":1.05}`)
		body := `{"model":"llama","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`
		var wg sync.WaitGroup
		codes := make(chan int, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp := e.do("POST", "/v1/chat/completions", key, body)
				readAll(t, resp)
				codes <- resp.StatusCode
			}()
		}
		// Wait until every request is either refused or waiting at vLLM.
		deadline := time.Now().Add(5 * time.Second)
		for len(codes)+up.count() < 20 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		close(up.gate)
		wg.Wait()
		close(codes)
		ok := 0
		for c := range codes {
			if c == 200 {
				ok++
			}
		}
		e.srv.Close(context.Background())
		var spent float64
		for _, r := range e.st.Records() {
			spent += r.Cost
		}
		if soft {
			if spent <= 1.05 {
				t.Fatalf("control: the old check admitted %d requests, %.3f spent: it cannot show an overshoot", ok, spent)
			}
			continue
		}
		if ok != 5 || spent > 1.05 {
			t.Fatalf("%d admitted, %.3f spent of 1.05", ok, spent)
		}
		st, _ := e.st.Ledger(context.Background(), store.BudgetKey{Kind: store.ScopeTeam, Name: "etabA"}, store.Day.Start(time.Now()))
		if st.Spent != 5*164_000_000 || st.Granted != 0 {
			t.Fatalf("ledger after the burst %+v", st)
		}
	}
}

// Lowering a budget below what the period committed is refused unless
// forced, and forcing belongs to proxy and organisation admins.
func TestLowerBudgetRefusedUnlessForced(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	e.check(nil, []call{{"master", "POST", "/admin/teams/etabA/projects", `{"name":"eval"}`, 201}})
	proj := e.createKey(`{"team":"etabA","project":"eval"}`)
	tadmin := e.adminToken("ta", `{"role":"team_admin","team":"etabA"}`)
	oadmin := e.adminToken("oa", `{"role":"org_admin","org":"etabA"}`)
	e.setBudget(`{"kind":"project","name":"etabA/eval","period":"day","max_cost":1}`)
	expect(t, "spend", e.chatWith(proj, `{"model":"llama","max_tokens":20,"messages":[]}`), 200)
	e.waitRecords(1)
	low := `{"kind":"project","name":"etabA/eval","period":"day","max_cost":0.1`
	e.check(map[string]string{"ta": tadmin, "oa": oadmin}, []call{
		{"ta", "PUT", "/admin/budgets", low + `}`, 409},
		{"ta", "PUT", "/admin/budgets", low + `,"force":true}`, 403},
		{"oa", "PUT", "/admin/budgets", low + `,"force":true}`, 200},
		{"ta", "PUT", "/admin/budgets", `{"kind":"project","name":"etabA/eval","period":"day","max_cost":0.15}`, 200}, // raising is never refused
	})
	expect(t, "over a forced limit", e.chatWith(proj, `{"model":"llama","max_tokens":1,"messages":[]}`), 429)
	out := readAll(t, e.do("GET", "/admin/budgets", master, ""))
	if !strings.Contains(out, `"committed":0.164`) {
		t.Fatalf("listing without the ledger: %s", out)
	}
}

// A user budget follows the person across their keys, is set by their
// team's admin and read by them, not by a colleague; every answer under a
// budget says when the tightest one resets.
func TestUserBudget(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	alice := e.adminToken("alice", `{"role":"member","team":"etabA"}`)
	bob := e.adminToken("bob", `{"role":"member","team":"etabA"}`)
	tadmin := e.adminToken("ta", `{"role":"team_admin","team":"etabA"}`)
	oadminB := e.adminToken("ob", `{"role":"org_admin","org":"etabB"}`)
	toks := map[string]string{"alice": alice, "bob": bob, "ta": tadmin, "ob": oadminB}
	budget := `{"kind":"user","name":"alice","period":"day","max_cost":0.4}`
	e.check(toks, []call{
		{"bob", "PUT", "/admin/budgets", budget, 403},
		{"ob", "PUT", "/admin/budgets", budget, 403},
		{"alice", "PUT", "/admin/budgets", budget, 403},
		{"ta", "PUT", "/admin/budgets", budget, 200},
	})
	for who, sees := range map[string]bool{"alice": true, "bob": false, "ta": true} {
		out := readAll(t, e.do("GET", "/admin/budgets", toks[who], ""))
		if strings.Contains(out, `"name":"alice"`) != sees {
			t.Errorf("%s sees alice's budget: %v (%s)", who, !sees, out)
		}
	}
	key := func(tok string) string {
		resp := e.do("POST", "/admin/keys", tok, `{"team":"etabA","name":"k"}`)
		var k struct{ Key string }
		if err := json.Unmarshal([]byte(readAll(t, resp)), &k); err != nil || resp.StatusCode != 201 {
			t.Fatalf("key: %d %v", resp.StatusCode, err)
		}
		return k.Key
	}
	k1, k2, kb := key(alice), key(alice), key(bob)
	tomorrow := store.Day.Start(time.Now()).AddDate(0, 0, 1).Format(time.RFC3339)
	body := `{"model":"llama","max_tokens":20,"messages":[]}`
	for i, k := range []string{k1, k2} { // 0.208 each at worst: two fit 0.4... after the first costs 0.164
		resp := e.do("POST", "/v1/chat/completions", k, body)
		readAll(t, resp)
		if resp.StatusCode != 200 || resp.Header.Get("X-Llmflow-Budget-Reset") != tomorrow {
			t.Fatalf("alice's key %d: %d reset %q", i+1, resp.StatusCode, resp.Header.Get("X-Llmflow-Budget-Reset"))
		}
	}
	resp := e.do("POST", "/v1/chat/completions", k1, body)
	if out := readAll(t, resp); resp.StatusCode != 429 || !strings.Contains(out, "user alice") || resp.Header.Get("X-Llmflow-Budget-Reset") != tomorrow {
		t.Fatalf("alice over her budget, whichever key: %d %q %s", resp.StatusCode, resp.Header.Get("X-Llmflow-Budget-Reset"), out)
	}
	resp = e.do("POST", "/v1/chat/completions", kb, body)
	if readAll(t, resp); resp.StatusCode != 200 || resp.Header.Get("X-Llmflow-Budget-Reset") != "" {
		t.Fatalf("bob has no budget: %d %q", resp.StatusCode, resp.Header.Get("X-Llmflow-Budget-Reset"))
	}
}

// release-replica gives back the grants of a replica that is gone, and is
// audited.
func TestReleaseReplica(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":1}`)
	k := store.BudgetKey{Kind: store.ScopeTeam, Name: "etabA"}
	now := time.Now()
	if _, err := e.st.GrowGrants(context.Background(), store.Holder{Replica: "ghost", Incarnation: "x"}, now, time.Minute,
		[]store.GrantRequest{{Key: k, Start: store.Day.Start(now), Need: 900_000_000}}); err != nil {
		t.Fatal(err)
	}
	key := e.createKey(`{"team":"etabA"}`)
	expect(t, "the dead replica holds the room", e.chatWith(key, `{"model":"llama","max_tokens":20,"messages":[]}`), 429)
	e.check(nil, []call{
		{"master", "POST", "/admin/budgets/release-replica", `{"replica":"` + e.srv.escrow.holder.Replica + `"}`, 409},
		{"master", "POST", "/admin/budgets/release-replica", `{"replica":"ghost"}`, 200},
	})
	expect(t, "after the release", e.chatWith(key, `{"model":"llama","max_tokens":20,"messages":[]}`), 200)
	if !e.audited("POST /admin/budgets/release-replica", "/admin/budgets/release-replica") {
		t.Fatal("release not audited")
	}
}
