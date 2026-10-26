package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
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
	sk := writeFile(t, dir, "salt", "0123456789abcdef0123456789abcdef-salt\n")
	return writeFile(t, dir, "c.hcl", `
listen             = "127.0.0.1:0"
master_key_file    = "`+mk+`"
cache_salt_key_file = "`+sk+`"
metrics_token_file = "`+mt+`"
org "o" {
  share = 1
  team "a" {}
}
pool "p" { capacity = 4 }
model "m" {
  pool = "p"
  upstream "u" {
    base_url = "http://127.0.0.1:1"
    key_file = "`+vk+`"
  }
}
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
	cfg := gatewayConfig(t, "store {\n  kind = \"postgres\"\n  dsn_file = \"/nonexistent\"\n  spool_dir = \""+t.TempDir()+"\"\n}\n")
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
	out, err = run(t, "export", "-c", cfg, "--from", "2026-10-01", "--to", "2026-11-01", "--group-by", "org,tag", "--format", "parsable2")
	if err != nil || !strings.HasPrefix(out, "from|to|org|tag|requests") {
		t.Fatalf("parsable2: %q %v", out, err)
	}
	out, err = run(t, "export", "-c", cfg, "--from", "2026-10-01", "--to", "2026-11-01", "--format", "json")
	if err != nil || !strings.Contains(out, `"rows":[]`) {
		t.Fatalf("json: %q %v", out, err)
	}
	for _, args := range [][]string{
		{"--from", "bad", "--to", "2026-11-01"},
		{"--from", "2026-10-01", "--to", "bad"},
		{"--from", "2026-11-01", "--to", "2026-10-01"},
		{"--from", "2026-10-01", "--to", "2026-11-01", "--group-by", "planet"},
		{"--from", "2026-10-01", "--to", "2026-11-01", "--format", "xml"},
	} {
		if _, err := run(t, append([]string{"export", "-c", cfg}, args...)...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, err := run(t, "export", "-c", "/nonexistent", "--from", "2026-10-01", "--to", "2026-11-01"); err == nil {
		t.Fatal("missing config accepted")
	}
}

// The CLI export has the API's bounds (366 days, 100 000 rows, 60 s),
// raised only by a flag, and every run is audited; a raised bound fails
// closed when its entry cannot be written.
func TestExportBounds(t *testing.T) {
	cfg := gatewayConfig(t, "")
	mem := store.NewMemory()
	old := exportStore
	exportStore = func(context.Context, *config.Config) (store.Store, error) { return nopClose{mem}, nil }
	defer func() { exportStore = old }()
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var recs []store.Record
	for i := 0; i < 3; i++ {
		recs = append(recs, store.Record{Org: "o", Team: "a", User: fmt.Sprint("u", i), Model: "m", Pool: "p", Cost: 1, Time: day})
	}
	if err := mem.InsertRecords(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	long := []string{"export", "-c", cfg, "--from", "2024-01-01", "--to", "2026-01-01"}
	if _, err := run(t, long...); err == nil || !strings.Contains(err.Error(), "--max-days") {
		t.Fatalf("two years accepted: %v", err)
	}
	if _, err := run(t, append(long, "--max-days", "800")...); err != nil {
		t.Fatalf("raised: %v", err)
	}
	byUser := []string{"export", "-c", cfg, "--from", "2026-10-01", "--to", "2026-11-01", "--group-by", "user"}
	if _, err := run(t, append(byUser, "--max-rows", "2")...); err == nil || !strings.Contains(err.Error(), "3 rows") {
		t.Fatalf("rows bound: %v", err)
	}
	for _, bad := range [][]string{{"--max-rows", "0"}, {"--max-days", "-1"}, {"--timeout", "0s"}} {
		if _, err := run(t, append(byUser, bad...)...); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, err := run(t, append(byUser, "--timeout", "1ns")...); err == nil {
		t.Error("a 1 ns deadline did not stop the report")
	}
	es, _ := mem.AuditRange(context.Background(), 1, 100)
	raised := 0
	for _, e := range es {
		if e.Kind != "export" || !strings.HasPrefix(e.Actor, "cli") {
			t.Fatalf("entry %+v", e)
		}
		if strings.Contains(e.After, `"raised":true`) {
			raised++
		}
	}
	if len(es) != 3 || raised != 1 {
		t.Fatalf("audited %d runs, %d raised: %+v", len(es), raised, es)
	}
	// The audit log fails: a default run warns, a raised one refuses.
	mem.SetFail(errors.New("store down"))
	if _, err := run(t, append(long, "--max-days", "800")...); err == nil || !strings.Contains(err.Error(), "audit entry") {
		t.Fatalf("raised without audit: %v", err)
	}
}

// nopClose keeps the shared test store open across runs.
type nopClose struct{ store.Store }

func (nopClose) Close() {}
