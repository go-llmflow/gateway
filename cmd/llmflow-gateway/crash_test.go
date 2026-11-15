package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
	"github.com/jackc/pgx/v5"
)

// Chaos, security audit follow-up (v0.10.0): the gateway process is killed
// with SIGKILL (by PID) while PostgreSQL is unreachable and usage records
// sit in its accounting spool. A new process on the same spool directory
// adopts the dead one's file and stores every record, once.
func TestCrashWithSpooledRecordsIsReplayed(t *testing.T) {
	admin := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if admin == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	// A database of its own.
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "crash_" + hex.EncodeToString(b[:])
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), admin); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	direct := u.String()
	px := newCutProxy(t, u.Host)
	u.Host = px.addr()
	proxied := u.String()

	// A fake vLLM answering every chat completion with a usage.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	defer up.Close()

	dir := t.TempDir()
	spoolDir := filepath.Join(dir, "spool")
	mk := writeFile(t, dir, "master", "m-secret-0123456789\n")
	vk := writeFile(t, dir, "vllm", "v-secret\n")
	sk := writeFile(t, dir, "salt", "0123456789abcdef0123456789abcdef-salt\n")
	dsn := writeFile(t, dir, "dsn", proxied+"\n")
	cfg := writeFile(t, dir, "c.hcl", `
listen              = "127.0.0.1:0"
master_key_file     = "`+mk+`"
cache_salt_key_file = "`+sk+`"
budget_refresh      = "1h"
key_cache_ttl       = "1h"
org "o" {
  share = 1
  team "a" {}
}
pool "p" { capacity = 64 }
model "m" {
  pool = "p"
  cost {
    input_per_mtok  = 1000
    output_per_mtok = 1000
  }
  upstream "u" {
    base_url = "`+up.URL+`"
    key_file = "`+vk+`"
  }
}
store {
  kind      = "postgres"
  dsn_file  = "`+dsn+`"
  spool_dir = "`+spoolDir+`"
}
`)
	bin := filepath.Join(t.TempDir(), "llmflow-gateway")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	first, addr := startGateway(t, bin, cfg)
	key := createKey(t, addr)
	chatOK(t, addr, key)                // warm: the key is cached,
	time.Sleep(1500 * time.Millisecond) // and its record stored

	px.cut() // PostgreSQL unreachable from now on
	const n = 20
	for i := 0; i < n; i++ {
		chatOK(t, addr, key)
	}
	time.Sleep(2500 * time.Millisecond) // flushed, fsync'd, insert refused
	if sp := metric(t, addr, "llmflow_accounting_spooled"); sp != n {
		t.Fatalf("spooled before the crash: %v, want %d", sp, n)
	}
	if err := first.Process.Kill(); err != nil { // SIGKILL, by PID
		t.Fatal(err)
	}
	_ = first.Wait()
	files, _ := os.ReadDir(spoolDir)
	if len(files) != 1 {
		t.Fatalf("spool files after the crash: %v", files)
	}

	px.restore()
	second, addr2 := startGateway(t, bin, cfg)
	defer func() { _ = second.Process.Kill(); _ = second.Wait() }()
	st, err := store.OpenPostgres(ctx, direct)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	deadline := time.Now().Add(20 * time.Second)
	var ids []string
	for {
		ids, err = st.UsageRecordIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) >= n+1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	t.Logf("records stored after the crash and restart: %d of %d, distinct %d; replayed %v",
		len(ids), n+1, len(seen), metric(t, addr2, "llmflow_accounting_replayed_total"))
	if len(ids) != n+1 || len(seen) != n+1 {
		t.Fatalf("stored %d (distinct %d), want %d once each", len(ids), len(seen), n+1)
	}
	if left, _ := os.ReadDir(spoolDir); len(left) != 1 { // the new process's own file only
		t.Fatalf("spool files after the replay: %v", left)
	}
}

// gatewayStartTimeout bounds the wait for "listening on": a race-instrumented
// suite on a loaded arm64 runner took more than the 20 s this used to be
// (gateway#176). It is a ceiling for a start that never comes, not a
// duration: a start that takes long is reported by startGateway's log line.
const gatewayStartTimeout = 90 * time.Second

// tailBuffer keeps the last bytes written to it (a child's output, bounded).
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

const tailBufferMax = 8 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > tailBufferMax {
		t.b = append([]byte(nil), t.b[len(t.b)-tailBufferMax:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// launchGateway starts the gateway and waits for "listening on <addr>". It
// returns early, with the child's output, when the child exits (its stdout
// closes) instead of waiting out the timeout, and on timeout it reports how
// long it waited and what the child had printed. The child's stderr and the
// non-"listening" lines of its stdout are kept (bounded).
func launchGateway(bin, cfg string, timeout time.Duration) (*exec.Cmd, string, time.Duration, error) {
	cmd := exec.Command(bin, "serve", "--config", cfg)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", 0, err
	}
	var tail tailBuffer
	cmd.Stderr = &tail
	begin := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, "", 0, err
	}
	lines := make(chan string, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if a, ok := strings.CutPrefix(sc.Text(), "listening on "); ok {
				select {
				case lines <- a:
				default:
				}
				continue
			}
			_, _ = tail.Write([]byte(sc.Text() + "\n"))
		}
	}()
	select {
	case a := <-lines:
		return cmd, strings.TrimSpace(a), time.Since(begin), nil
	case <-exited:
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, "", time.Since(begin), fmt.Errorf("the gateway exited after %v before listening; its output:\n%s", time.Since(begin).Round(time.Millisecond), tail.String())
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-exited // the pipe is read to its end before Wait closes it
		_ = cmd.Wait()
		return nil, "", time.Since(begin), fmt.Errorf("the gateway was not listening after %v; its output so far:\n%s", timeout, tail.String())
	}
}

func startGateway(t *testing.T, bin, cfg string) (*exec.Cmd, string) {
	t.Helper()
	cmd, addr, took, err := launchGateway(bin, cfg, gatewayStartTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("gateway %s listening after %v", filepath.Base(cfg), took.Round(time.Millisecond))
	return cmd, addr
}

func createKey(t *testing.T, addr string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://"+addr+"/admin/keys", strings.NewReader(`{"team":"a"}`))
	req.Header.Set("Authorization", "Bearer m-secret-0123456789")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var k struct{ Key string }
	if err := json.NewDecoder(resp.Body).Decode(&k); err != nil || resp.StatusCode != 201 {
		t.Fatalf("create key: %d %v", resp.StatusCode, err)
	}
	return k.Key
}

func chatOK(t *testing.T, addr, key string) {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d", resp.StatusCode)
	}
}

// metric reads one unlabelled sample from /metrics (loopback is allowed).
func metric(t *testing.T, addr, name string) float64 {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), name+" "); ok {
			var f float64
			fmt.Sscan(v, &f)
			return f
		}
	}
	return -1
}

// cutProxy forwards TCP to target until cut: then it resets every open
// connection and refuses new ones, until restore.
type cutProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	down   bool
	conns  map[net.Conn]bool
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln, target: target, conns: map[net.Conn]bool{}}
	t.Cleanup(func() { ln.Close(); p.cut() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			down := p.down
			p.mu.Unlock()
			if down {
				c.Close()
				continue
			}
			go p.pipe(c)
		}
	}()
	return p
}

func (p *cutProxy) addr() string { return p.ln.Addr().String() }

func (p *cutProxy) pipe(c net.Conn) {
	u, err := net.Dial("tcp", p.target)
	if err != nil {
		c.Close()
		return
	}
	p.mu.Lock()
	p.conns[c], p.conns[u] = true, true
	p.mu.Unlock()
	go func() { _, _ = io.Copy(u, c); u.Close(); c.Close() }()
	_, _ = io.Copy(c, u)
	c.Close()
	u.Close()
}

func (p *cutProxy) cut() {
	p.mu.Lock()
	p.down = true
	for c := range p.conns {
		c.Close()
		delete(p.conns, c)
	}
	p.mu.Unlock()
}

func (p *cutProxy) restore() {
	p.mu.Lock()
	p.down = false
	p.mu.Unlock()
}
