package gateway

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// A client that closes the connection after the last content delta, before
// the final usage chunk, must still be charged for the work vLLM did:
// otherwise it uses the GPUs for free and keeps its fair-share level high.
func TestStreamAbortedBeforeUsageIsCharged(t *testing.T) {
	const n = 5
	release := make(chan struct{})
	defer close(release)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
			fl.Flush()
		}
		// The usage chunk comes last, after the client has left.
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"total_tokens\":105}}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	e := newEnv(t, testConfig(up.URL))
	_, key := e.newKey("etabA")
	if resp := e.do("PUT", "/admin/budgets", master, `{"kind":"team","name":"etabA","period":"day","max_cost":1000}`); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("budget %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"llama","stream":true,"messages":[{"role":"user","content":"`+strings.Repeat("x", 400)+`"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	got := 0
	for got < n && sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			got++
		}
	}
	resp.Body.Close() // gone before the usage chunk
	if got != n {
		t.Fatalf("read %d deltas", got)
	}
	waitIdle(t, e)

	if u := e.srv.pools["b300"].snapshot()[0].Usage; !(u > 0) {
		t.Fatalf("fair-share usage not charged: %v", u)
	}
	if spent, ok := e.srv.budget.spent(store.BudgetKey{Kind: store.ScopeTeam, Name: "etabA"}); !ok || !(spent > 0) {
		t.Fatalf("budget not charged: %v", spent)
	}
	e.srv.Close(context.Background())
	recs, _ := e.st.Report(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{"team", "pool"})
	if len(recs) != 1 || recs[0].EstimatedRequests != 1 || recs[0].CompletionTokens != n || recs[0].PromptTokens <= 100 {
		t.Fatalf("record %+v", recs)
	}
	if m := readAll(t, e.do("GET", "/metrics", "", "")); !strings.Contains(m, `llmflow_usage_estimated_total{team="etabA",model="llama"} 1`) {
		t.Fatal("estimated metric missing")
	}
}

// A non-streaming client gone while vLLM generates is charged its prompt.
func TestNonStreamAbortChargesPrompt(t *testing.T) {
	up := newFakeVLLM(t)
	up.gate = make(chan struct{})
	defer close(up.gate)
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA")
	client := &http.Client{Timeout: 200 * time.Millisecond}
	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/chat/completions", strings.NewReader(chat))
	req.Header.Set("Authorization", "Bearer "+key)
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("expected a client timeout")
	}
	waitIdle(t, e)
	e.srv.Close(context.Background())
	recs, _ := e.st.Report(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{"team", "pool"})
	if len(recs) != 1 || recs[0].EstimatedRequests != 1 || recs[0].PromptTokens == 0 || recs[0].CompletionTokens != 0 || !(recs[0].GPUHours > 0) {
		t.Fatalf("record %+v", recs)
	}
}
