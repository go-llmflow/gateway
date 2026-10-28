package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// Each chat request of the fake upstream costs 0.164 GPU-hours with the
// test prices: (60 uncached × 1000 + 40 cached × 100 + 20 × 5000) / 1e6,
// or less when its max_tokens is below 20 (the fake honours it).
//
// Hard budgets (#87): its worst case, with max_model_len 128 (the fake's
// /v1/models) and max_tokens M, is W = 0.128 + 0.004·M. Without a
// max_tokens the gateway picks the largest M that fits the room left, at
// least 16 (W = 0.192): a budget of 0.2 admits one request (M = 18, cost
// 0.154), a budget of 0.4 two.

func (e *env) chatWith(key, body string, hdr ...string) int {
	e.t.Helper()
	resp := e.do("POST", "/v1/chat/completions", key, body, hdr...)
	readAll(e.t, resp)
	return resp.StatusCode
}

func (e *env) setBudget(body string) {
	e.t.Helper()
	resp := e.do("PUT", "/admin/budgets", master, body)
	if out := readAll(e.t, resp); resp.StatusCode != 200 {
		e.t.Fatalf("set budget %s: %d %s", body, resp.StatusCode, out)
	}
}

func (e *env) createKey(body string) string {
	e.t.Helper()
	resp := e.do("POST", "/admin/keys", master, body)
	out := readAll(e.t, resp)
	var k struct{ Key string }
	if err := json.Unmarshal([]byte(out), &k); err != nil || resp.StatusCode != 201 {
		e.t.Fatalf("create key %s: %d %s", body, resp.StatusCode, out)
	}
	return k.Key
}

// waitRecords waits until the store holds n usage records.
func (e *env) waitRecords(n int) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	settled := func() bool {
		e.srv.tracker.mu.Lock()
		defer e.srv.tracker.mu.Unlock()
		return len(e.srv.tracker.pending) == 0
	}
	// Stored, and settled for the arming tracker (the recorder untracks
	// just after the insert).
	for len(e.st.Records()) < n || !settled() {
		if time.Now().After(deadline) {
			e.t.Fatalf("%d records stored, want %d (settled: %v)", len(e.st.Records()), n, settled())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func expect(t *testing.T, what string, got int, want ...int) {
	t.Helper()
	for _, w := range want {
		if got == w {
			return
		}
	}
	t.Fatalf("%s: %d, want %v", what, got, want)
}

func TestTagBudgets(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Tags = config.Tags{Header: "X-Llmflow-Tags", Allowed: []string{"eval", "prod"}}
	e := newEnv(t, cfg)
	tagged := e.createKey(`{"team":"etabA","tags":["eval"]}`)
	plain := e.createKey(`{"team":"etabA"}`)
	e.setBudget(`{"kind":"tag","name":"eval","period":"day","max_cost":0.4}`)
	e.setBudget(`{"kind":"tag","name":"prod","period":"day","max_cost":0.2}`)

	expect(t, "first eval", e.chatWith(tagged, chat), 200)
	expect(t, "second eval (0.236 left ≥ 0.192)", e.chatWith(tagged, chat), 200)
	expect(t, "third eval", e.chatWith(tagged, chat), 429)
	expect(t, "untagged key", e.chatWith(plain, chat), 200)
	// Request tags from the allow-listed header.
	expect(t, "untagged key tagged eval by header", e.chatWith(plain, chat, "X-Llmflow-Tags", "eval"), 429)
	expect(t, "first prod", e.chatWith(plain, chat, "X-Llmflow-Tags", " prod "), 200)
	expect(t, "second prod", e.chatWith(plain, chat, "X-Llmflow-Tags", "prod"), 429)
	expect(t, "tag not allowed", e.chatWith(plain, chat, "X-Llmflow-Tags", "secret"), 400)
	expect(t, "too many tags", e.chatWith(plain, chat, "X-Llmflow-Tags", "prod,eval,prod,eval,prod,eval,prod,eval,prod"), 400)
	resp := e.do("POST", "/admin/keys", master, `{"team":"etabA","tags":["a b"]}`)
	if readAll(t, resp); resp.StatusCode != 400 {
		t.Fatalf("bad key tag %d", resp.StatusCode)
	}
	e.srv.Close(context.Background())
	if recs := e.st.Records(); len(recs) == 0 || strings.Join(recs[0].Tags, ",") != "eval" {
		t.Fatalf("record tags %+v", recs)
	}
}

func TestModelAndScopeBudgets(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	e.check(nil, []call{{"master", "POST", "/admin/teams/etabA/projects", `{"name":"eval"}`, 201}})
	key := e.createKey(`{"team":"etabA"}`)
	proj := e.createKey(`{"team":"etabA","project":"eval"}`)
	var ks []struct{ ID, Project string }
	_ = json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/keys?team=etabA", master, ""))), &ks)
	var keyID string
	for _, k := range ks {
		if k.Project == "" {
			keyID = k.ID
		}
	}
	batch := strings.Replace(chat, `"llama"`, `"llama-batch"`, 1)
	e.setBudget(fmt.Sprintf(`{"kind":"key","name":%q,"model":"llama-batch","period":"week","max_cost":0.2}`, keyID))
	e.setBudget(`{"kind":"project","name":"etabA/eval","period":"month","max_cost":0.2}`)

	expect(t, "batch 1", e.chatWith(key, batch), 200)
	expect(t, "batch 2", e.chatWith(key, batch), 429)
	expect(t, "other model unaffected", e.chatWith(key, chat), 200)
	expect(t, "project 1", e.chatWith(proj, chat), 200)
	expect(t, "project 2", e.chatWith(proj, chat), 429)
	expect(t, "team key outside the project", e.chatWith(key, chat), 200)

	// An organisation budget covers every team of the organisation; created
	// mid-period, it starts from the usage already stored.
	e.waitRecords(4)
	e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":0.5}`)
	expect(t, "org exhausted", e.chatWith(key, chat), 429)
	out := readAll(t, e.do("GET", "/admin/budgets", master, ""))
	if !strings.Contains(out, `"kind":"org"`) || !strings.Contains(out, `"spent":0.`) {
		t.Fatalf("budget listing %s", out)
	}
	resp := e.do("POST", "/v1/chat/completions", key, chat)
	if body := readAll(t, resp); !strings.Contains(body, "org etabA") {
		t.Fatalf("error does not name the scope: %s", body)
	}
}

func TestTemporaryIncrease(t *testing.T) {
	up := newFakeVLLM(t)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := newEnv(t, testConfig(up.URL()), WithClock(clock))
	key := e.createKey(`{"team":"etabA"}`)
	e.setBudget(`{"kind":"team","name":"etabA","period":"month","max_cost":0.2}`)
	expect(t, "first", e.chatWith(key, chat), 200)
	expect(t, "exhausted", e.chatWith(key, chat), 429)

	exp := now.Add(time.Hour).UTC().Format(time.RFC3339)
	for _, bad := range []string{
		`{"kind":"team","name":"etabA","amount":0,"expires_at":"` + exp + `"}`,
		`{"kind":"team","name":"etabA","amount":1,"expires_at":"2000-01-01T00:00:00Z"}`,
		`{"kind":"team","name":"etabA","amount":1,"expires_at":"2999-01-01T00:00:00Z"}`,
	} {
		resp := e.do("POST", "/admin/budgets/increase", master, bad)
		if readAll(t, resp); resp.StatusCode != 400 {
			t.Fatalf("bad increase %s: %d", bad, resp.StatusCode)
		}
	}
	resp := e.do("POST", "/admin/budgets/increase", master, `{"kind":"team","name":"etabB","amount":1,"expires_at":"`+exp+`"}`)
	if readAll(t, resp); resp.StatusCode != 404 {
		t.Fatalf("increase of a missing budget %d", resp.StatusCode)
	}
	resp = e.do("POST", "/admin/budgets/increase", master, `{"kind":"team","name":"etabA","amount":1,"expires_at":"`+exp+`"}`)
	if out := readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("increase %d %s", resp.StatusCode, out)
	}
	expect(t, "during the increase", e.chatWith(key, chat), 200)
	// Re-setting the budget keeps the increase.
	e.setBudget(`{"kind":"team","name":"etabA","period":"month","max_cost":0.2}`)
	expect(t, "after re-set", e.chatWith(key, chat), 200)
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	if err := e.srv.budget.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	expect(t, "after expiry", e.chatWith(key, chat), 429)
}

func TestSoftBudgetAlertsOncePerPeriod(t *testing.T) {
	up := newFakeVLLM(t)
	var mu sync.Mutex
	var hooks []alertPayload
	var auth string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p alertPayload
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &p)
		mu.Lock()
		hooks = append(hooks, p)
		auth = r.Header.Get("Authorization")
		mu.Unlock()
	}))
	defer hook.Close()
	cfg := testConfig(up.URL())
	cfg.Alerts = config.Alerts{WebhookURL: hook.URL, SMTP: &config.SMTP{Server: "relay.example.org:25", From: "llmflow@example.org", To: []string{"ops@example.org"}}}
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), WebhookToken: "hook-secret", CacheSaltKey: testSaltKey})
	if err != nil {
		t.Fatal(err)
	}
	var mails []string
	srv.alerts.sendMail = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		mu.Lock()
		mails = append(mails, string(msg))
		mu.Unlock()
		return nil
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	e := &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	t.Cleanup(func() { gw.Close(); srv.Close(context.Background()) })

	key := e.createKey(`{"team":"etabA"}`)
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":10,"soft_limit":0.1}`)
	expect(t, "request", e.chatWith(key, chat), 200)
	// Wait until the accounting record reaches the store.
	deadline := time.Now().Add(5 * time.Second)
	for len(st.Records()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		if err := srv.budget.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n, m := len(hooks), len(mails)
		mu.Unlock()
		if n > 0 && m > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hooks) != 1 || hooks[0].Budget.Name != "etabA" || hooks[0].SoftLimit != 0.1 || !(hooks[0].Spent >= 0.1) || auth != "Bearer hook-secret" {
		t.Fatalf("webhook calls %+v auth %q", hooks, auth)
	}
	if len(mails) != 1 || !strings.Contains(mails[0], "Subject: [llmflow] soft budget reached: team etabA\r\n") ||
		!strings.Contains(mails[0], "To: ops@example.org\r\n") {
		t.Fatalf("mails %q", mails)
	}
	if m := readAll(t, e.do("GET", "/metrics", "", "")); !strings.Contains(m, `llmflow_soft_budget_alerts_total{kind="team"} 1`) {
		t.Fatal("alert metric missing")
	}
}

// A refresh before the accounting writer has stored a request's record
// must not forget the request's cost (the first version did).
func TestRefreshKeepsUnwrittenSpend(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	key := e.createKey(`{"team":"etabA"}`)
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":0.2}`)
	expect(t, "first", e.chatWith(key, chat), 200)
	if len(e.st.Records()) != 0 {
		t.Skip("record already written; the race this test needs did not happen")
	}
	if err := e.srv.budget.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	expect(t, "after a refresh that cannot see the record yet", e.chatWith(key, chat), 429)
	// Once written and refreshed, the store carries it.
	e.srv.Close(context.Background())
	if err := e.srv.budget.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spent, _ := e.srv.budget.spent(store.BudgetKey{Kind: store.ScopeTeam, Name: "etabA"}); spent < 0.154 || spent > 0.155 {
		t.Fatalf("spent after write and refresh: %v (counted twice or lost)", spent)
	}
}

func TestHeaderSafe(t *testing.T) {
	if got := headerSafe("a\r\nBcc: x@y\x00z"); got != "aBcc: x@yz" {
		t.Fatalf("%q", got)
	}
}

func TestRequestScopeKeys(t *testing.T) {
	// A user: their user budget and their profile's weekly and session
	// budgets, or, beyond the profile, its overflow budget (#90).
	if n := len((reqScope{org: "o", team: "t", key: "k", user: "alice", model: "m"}).keys()); n != 12 {
		t.Fatalf("with a user: %d keys", n)
	}
	over := (reqScope{org: "o", team: "t", key: "k", user: "alice", model: "m", overflow: true}).keys()
	if len(over) != 10 || !inKeys(store.BudgetKey{Kind: store.ScopeProfileOverflow, Name: "alice"}, over) ||
		inKeys(store.BudgetKey{Kind: store.ScopeProfileWeek, Name: "alice"}, over) {
		t.Fatalf("overflow: %+v", over)
	}
	r := reqScope{org: "o", team: "t", project: "p", key: "k", model: "m", tags: []string{"x"}}
	ks := r.keys()
	if len(ks) != 10 || ks[1].Model != "m" || ks[0].Model != "" {
		t.Fatalf("%+v", ks)
	}
	if len((reqScope{org: "o", team: "t", key: "k"}).keys()) != 6 {
		t.Fatal("no project, no tags")
	}
}

// rowKeys is the index the budgeter's refresh counts spend with: it must
// list exactly the keys rowMatches accepts, or a budget would be counted
// wrong (the index replaced matching every row against every budget).
func TestRowKeysAreMatches(t *testing.T) {
	rows := []store.SpendRow{
		{Org: "o", Team: "t", KeyID: "k", Model: "m"},
		{Org: "o", Team: "t", Project: "p", KeyID: "k", User: "alice", Model: "m", Tags: []string{"x", "y"}, Unit: "USD"},
		{Org: "o", Team: "t", KeyID: "k", User: "alice", Model: "m", Overflow: true},
		{Org: "o", Team: "t", KeyID: "k", User: "bob", Model: ""},
	}
	var cands []store.BudgetKey
	for _, kind := range []store.ScopeKind{store.ScopeOrg, store.ScopeTeam, store.ScopeProject, store.ScopeKey, store.ScopeUser, store.ScopeTag,
		store.ScopeProfileWeek, store.ScopeProfileSession, store.ScopeProfileOverflow} {
		for _, name := range []string{"o", "t", "t/p", "k", "alice", "bob", "x", "y", "z"} {
			for _, model := range []string{"", "m", "n"} {
				for _, unit := range []string{"", "USD"} {
					cands = append(cands, store.BudgetKey{Kind: kind, Name: name, Model: model, Unit: unit})
				}
			}
		}
	}
	for _, r := range rows {
		ks := rowKeys(r)
		seen := map[store.BudgetKey]bool{}
		for _, k := range ks {
			if seen[k] {
				t.Fatalf("%+v: %v twice", r, k)
			}
			seen[k] = true
		}
		for _, k := range cands {
			if rowMatches(k, r) != seen[k] {
				t.Fatalf("%+v: key %v matches=%v listed=%v", r, k, rowMatches(k, r), seen[k])
			}
		}
	}
}
