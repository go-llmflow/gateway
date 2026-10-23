package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// An upstream killed in the middle of a stream: the client receives what
// was produced, the gateway releases every counter, and nothing is charged
// (open defect 4 in the README).
func TestChaosUpstreamDiesMidStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer up.Close()
	e := newEnv(t, testConfig(up.URL))
	_, key := e.newKey("etabA")
	resp := e.do("POST", "/v1/chat/completions", key, `{"model":"llama","stream":true}`)
	out := readAll(t, resp)
	if !strings.Contains(out, "partial") {
		t.Fatalf("client lost the partial stream: %q", out)
	}
	waitIdle(t, e)
}

// Clients that disconnect while vLLM is still generating: counters return
// to zero and the upstream request is cancelled.
func TestChaosClientsDisconnect(t *testing.T) {
	up := newFakeVLLM(t)
	up.gate = make(chan struct{}) // never opened: requests hang until cancelled
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA")
	client := &http.Client{Timeout: 100 * time.Millisecond}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", e.gw.URL+"/v1/chat/completions", strings.NewReader(chat))
			req.Header.Set("Authorization", "Bearer "+key)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	waitIdle(t, e)
}

func waitIdle(t *testing.T, e *env) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		busy := 0
		for _, s := range e.srv.pools["b300"].Snapshot() {
			busy += s.InFlight + s.Borrowing
		}
		for _, u := range e.srv.groups["llama"].upstreams {
			busy += int(u.inflight.Load())
		}
		if busy == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("counters never returned to zero: %+v", e.srv.pools["b300"].Snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
