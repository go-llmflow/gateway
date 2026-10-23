package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// instantUpstream answers every completion at once, like the recording
// fake upstream LiteLLM was measured against (2026-10-23).
func instantUpstream() *httptest.Server {
	body := []byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

func post(b *testing.B, c *http.Client, url, key, body string) time.Duration {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	t := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		b.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	d := time.Since(t)
	if resp.StatusCode != 200 {
		b.Fatalf("status %d", resp.StatusCode)
	}
	return d
}

func pct(xs []time.Duration, p float64) time.Duration {
	s := append([]time.Duration(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(p*float64(len(s)-1))]
}

// BenchmarkOverhead interleaves one direct request to the fake upstream
// and one request through the gateway per iteration (same client, same
// keep-alive connections), and reports the latency percentiles of both
// and their difference: the gateway's added latency. Run it on one core
// with
//
//	go test -run '^$' -bench Overhead -cpu 1 -benchtime 20000x ./gateway
//
// The gateway runs with a memory store; budgets, limits, fair share,
// accounting and metrics are all on the path.
func BenchmarkOverhead(b *testing.B) {
	up := instantUpstream()
	defer up.Close()
	cfg := testConfig(up.URL)
	cfg.Health.Interval = time.Hour
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: map[string]string{"llama": "k", "llama-batch": "k"}})
	if err != nil {
		b.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer srv.Close(context.Background())
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()
	if err := st.CreateTeam(context.Background(), "etabA"); err != nil && err != store.ErrExists {
		b.Fatal(err)
	}
	secret, id, h := store.GenerateKey()
	if err := st.CreateKey(context.Background(), store.Key{ID: id, Hash: h, Team: "etabA"}); err != nil {
		b.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
	body := `{"model":"llama","messages":[{"role":"user","content":"Say hi."}],"max_tokens":8}`
	upstreamBody := strings.Replace(body, `"llama"`, `"meta/llama"`, 1)
	for i := 0; i < 200; i++ { // warm connections and caches
		post(b, client, up.URL+"/v1/chat/completions", "", upstreamBody)
		post(b, client, gw.URL+"/v1/chat/completions", secret, body)
	}
	direct := make([]time.Duration, 0, b.N)
	via := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			direct = append(direct, post(b, client, up.URL+"/v1/chat/completions", "", upstreamBody))
			via = append(via, post(b, client, gw.URL+"/v1/chat/completions", secret, body))
		} else {
			via = append(via, post(b, client, gw.URL+"/v1/chat/completions", secret, body))
			direct = append(direct, post(b, client, up.URL+"/v1/chat/completions", "", upstreamBody))
		}
	}
	b.StopTimer()
	us := func(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e3 }
	b.ReportMetric(us(pct(direct, 0.5)), "direct-p50-µs")
	b.ReportMetric(us(pct(direct, 0.99)), "direct-p99-µs")
	b.ReportMetric(us(pct(via, 0.5)), "gateway-p50-µs")
	b.ReportMetric(us(pct(via, 0.99)), "gateway-p99-µs")
	b.ReportMetric(us(pct(via, 0.5)-pct(direct, 0.5)), "added-p50-µs")
	b.ReportMetric(us(pct(via, 0.99)-pct(direct, 0.99)), "added-p99-µs")
	b.ReportMetric(0, "ns/op")
	if testing.Verbose() {
		fmt.Printf("internal overhead histogram count %d\n", srv.met.overhead.count)
	}
}
