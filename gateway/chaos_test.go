package gateway

import (
	"encoding/json"
	"fmt"
	"net"
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

// An upstream killed mid-stream, then restarted on the same address: the
// next requests are served (security audit 2026-10-26, chaos).
func TestChaosUpstreamRestart(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	f := newFakeVLLM(t)
	kill := make(chan struct{})
	first := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-kill
	})}}
	first.Start()
	e := newEnv(t, testConfig("http://"+addr))
	_, key := e.newKey("etabA")
	done := make(chan string)
	go func() {
		resp := e.do("POST", "/v1/chat/completions", key, `{"model":"llama","stream":true,"messages":[{"role":"user","content":"x"}]}`)
		done <- readAll(t, resp)
	}()
	time.Sleep(100 * time.Millisecond)
	first.CloseClientConnections() // the process dies mid-stream
	close(kill)
	first.Close()
	if out := <-done; !strings.Contains(out, "partial") {
		t.Fatalf("partial stream lost: %q", out)
	}
	// Restarted on the same address.
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("could not listen again on %s: %v", addr, err)
	}
	second := &httptest.Server{Listener: ln2, Config: &http.Server{Handler: http.HandlerFunc(f.serve)}}
	second.Start()
	defer second.Close()
	expect(t, "after the restart", e.chatWith(key, chat), 200)
	waitIdle(t, e)
}

// A key rotated (no grace period) while requests made with it are in
// flight: those finish and are charged to the key; the old secret is
// refused at once afterwards, the new one served.
func TestChaosRotationWhileInFlight(t *testing.T) {
	up := newFakeVLLM(t)
	up.gate = make(chan struct{})
	e := newEnv(t, testConfig(up.URL()))
	id, old := e.newKey("etabA")
	const n = 16
	var wg sync.WaitGroup
	statuses := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- e.chatWith(old, chat)
		}()
	}
	for up.count() < n {
		time.Sleep(5 * time.Millisecond)
	}
	resp := e.do("POST", "/admin/keys/"+id+"/rotate", master, "")
	var rot struct{ Key string }
	if err := json.Unmarshal([]byte(readAll(t, resp)), &rot); err != nil || resp.StatusCode != 200 {
		t.Fatalf("rotate: %d %v", resp.StatusCode, err)
	}
	close(up.gate)
	wg.Wait()
	close(statuses)
	for s := range statuses {
		if s != 200 {
			t.Errorf("a request in flight during the rotation: %d", s)
		}
	}
	expect(t, "old secret after rotation", e.chatWith(old, chat), 401)
	expect(t, "new secret", e.chatWith(rot.Key, chat), 200)
	charged := 0
	deadline := time.Now().Add(5 * time.Second)
	for charged < n+1 && time.Now().Before(deadline) {
		charged = 0
		for _, r := range e.st.Records() {
			if r.KeyID == id {
				charged++
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if charged != n+1 {
		t.Fatalf("records charged to the rotated key: %d, want %d", charged, n+1)
	}
	waitIdle(t, e)
}

// The gateway's clock steps backwards (NTP) between audit entries: the
// chain stays verifiable (entry times are recorded, not ordered).
func TestChaosAuditClockStepsBack(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()), WithClock(clock))
	e.check(nil, []call{{"master", "POST", "/admin/teams/etabA/projects", `{"name":"p1"}`, 201}})
	mu.Lock()
	now = now.Add(-2 * time.Hour)
	mu.Unlock()
	e.check(nil, []call{{"master", "POST", "/admin/teams/etabA/projects", `{"name":"p2"}`, 201}})
	resp := e.do("POST", "/admin/audit/verify", master, "")
	out := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("verify after a clock step back: %d %s", resp.StatusCode, out)
	}
}

func waitIdle(t *testing.T, e *env) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		busy := 0
		for _, s := range e.srv.pools["b300"].snapshot() {
			busy += s.InFlight + s.Borrowing
		}
		for _, u := range e.srv.groups["llama"].upstreams {
			busy += int(u.inflight.Load())
		}
		if busy == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("counters never returned to zero: %+v", e.srv.pools["b300"].snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
