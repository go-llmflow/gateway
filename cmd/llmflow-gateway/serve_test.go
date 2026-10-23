package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func gatewayConfig(t *testing.T, extra string) string {
	dir := t.TempDir()
	mk := writeFile(t, dir, "master", "m-secret\n")
	vk := writeFile(t, dir, "vllm", "v-secret\n")
	mt := writeFile(t, dir, "metrics", "mt\n")
	return writeFile(t, dir, "c.yaml", `
listen: 127.0.0.1:0
master_key_file: `+mk+`
metrics_token_file: `+mt+`
teams: [{name: a, share: 1}]
pools: [{name: p, capacity: 4}]
model_groups:
  - {name: m, pool: p, upstreams: ["http://127.0.0.1:1"], api_key_file: `+vk+`}
`+extra)
}

func TestServeStartsAndStops(t *testing.T) {
	cfg := gatewayConfig(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	old := signalContext
	signalContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	defer func() { signalContext = old }()

	cmd := RootCmd()
	out := &syncBuf{}
	cmd.SetOut(out)
	cmd.SetErr(&syncBuf{})
	cmd.SetArgs([]string{"serve", "--config", cfg})
	errc := make(chan error, 1)
	go func() { errc <- cmd.Execute() }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "listening on ") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	addr := strings.TrimSpace(strings.TrimPrefix(out.String(), "listening on "))
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestServeErrors(t *testing.T) {
	if _, err := run(t, "serve", "-c", "/nonexistent"); err == nil {
		t.Fatal("missing config accepted")
	}
	cfg := gatewayConfig(t, "store: {kind: postgres, dsn_file: /nonexistent}\n")
	if _, err := run(t, "serve", "-c", cfg); err == nil || !strings.Contains(err.Error(), "dsn_file") {
		t.Fatalf("missing dsn file: %v", err)
	}
}

func TestExport(t *testing.T) {
	cfg := gatewayConfig(t, "")
	out, err := run(t, "export", "-c", cfg, "--from", "2026-10-01", "--to", "2026-11-01")
	if err != nil || !strings.HasPrefix(out, "from,to,team,pool,requests") {
		t.Fatalf("%q %v", out, err)
	}
	for _, args := range [][]string{
		{"--from", "bad", "--to", "2026-11-01"},
		{"--from", "2026-10-01", "--to", "bad"},
		{"--from", "2026-11-01", "--to", "2026-10-01"},
	} {
		if _, err := run(t, append([]string{"export", "-c", cfg}, args...)...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, err := run(t, "export", "-c", "/nonexistent", "--from", "2026-10-01", "--to", "2026-11-01"); err == nil {
		t.Fatal("missing config accepted")
	}
}
