package main

import (
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Chaos for high availability (#74): two gateway processes share one pool
// through PostgreSQL. The owner of the pool is killed with SIGKILL (by
// PID) while requests are in flight on both replicas and traffic keeps
// arriving at the survivor. The survivor takes the lease with a higher
// epoch, keeps the reservation (the dead owner's request counts until
// max_request_duration), and every request it receives meanwhile is
// served.
func TestHAOwnerKilled(t *testing.T) {
	admin := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if admin == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "hacrash_" + hex.EncodeToString(b[:])
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

	// A fake vLLM: a request whose prompt says "hold" waits until released
	// (or until its gateway dies); every other one answers at once.
	var mu sync.Mutex
	hold := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "hold") {
			mu.Lock()
			h := hold
			mu.Unlock()
			select {
			case <-h:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	defer up.Close()

	dir := t.TempDir()
	mk := writeFile(t, dir, "master", "m-secret-0123456789\n")
	vk := writeFile(t, dir, "vllm", "v-secret\n")
	sk := writeFile(t, dir, "salt", "0123456789abcdef0123456789abcdef-salt\n")
	hk := writeFile(t, dir, "ha", "ha-shared-secret-0123456789abcdef0123456789\n")
	dsn := writeFile(t, dir, "dsn", u.String()+"\n")
	conf := func(id string) string {
		return writeFile(t, dir, id+".hcl", `
listen              = "127.0.0.1:0"
master_key_file     = "`+mk+`"
cache_salt_key_file = "`+sk+`"
budget_refresh      = "1h"
key_cache_ttl       = "1h"
limits { max_request_duration = "5s" }
org "a" {
  share = 0.5
  team "ta" {}
}
org "b" {
  share = 0.5
  team "tb" {}
}
pool "p" { capacity = 4 }
model "m" {
  pool = "p"
  upstream "u" {
    base_url = "`+up.URL+`"
    key_file = "`+vk+`"
  }
}
store {
  kind      = "postgres"
  dsn_file  = "`+dsn+`"
  spool_dir = "`+filepath.Join(dir, "spool-"+id)+`"
}
ha {
  replica_id      = "`+id+`"
  listen          = "127.0.0.1:`+freePort(t)+`"
  peer_networks   = ["127.0.0.0/8"]
  secret_file     = "`+hk+`"
  lease_ttl       = "1500ms"
  heartbeat       = "200ms"
  forward_timeout = "200ms"
  usage_persist   = "300ms"
}
`)
	}
	bin := filepath.Join(t.TempDir(), "llmflow-gateway")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cfgA, cfgB := conf("A"), conf("B")
	if out, err := run(t, "check-config", "-c", cfgA); err != nil {
		t.Fatalf("check-config: %v %s", err, out)
	}
	procA, addrA := startGateway(t, bin, cfgA)
	defer func() { _ = procA.Process.Kill(); _ = procA.Wait() }()
	procB, addrB := startGateway(t, bin, cfgB)
	defer func() { _ = procB.Process.Kill(); _ = procB.Wait() }()
	if metricL(t, addrA, `llmflow_ha_owner{pool="p"}`) != 1 || metricL(t, addrB, `llmflow_ha_owner{pool="p"}`) != 0 {
		t.Fatal("A, started first, should own the pool")
	}
	epoch1 := metricL(t, addrB, `llmflow_ha_epoch{pool="p"}`)
	key := createKeyFor(t, addrA, "ta")
	keyB := createKeyFor(t, addrA, "tb") // the background traffic: another org

	// Two held requests of org a on B (forwarded to A: within share), one
	// on A (a's reservation of 2 is full: borrowing).
	type res struct {
		status, prio int
	}
	held := make(chan res, 8)
	sendAs := func(k, addr, prompt string) (int, int) {
		req, _ := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"`+prompt+`"}]}`))
		req.Header.Set("Authorization", "Bearer "+k)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, -1
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		p, _ := strconv.Atoi(resp.Header.Get("X-Llmflow-Priority"))
		return resp.StatusCode, p
	}
	send := func(addr, prompt string) (int, int) { return sendAs(key, addr, prompt) }
	priorityOf := func(addr string) int {
		st, p := send(addr, "quick")
		if st != 200 {
			t.Fatalf("quick request on %s: %d", addr, st)
		}
		return p
	}
	for range 2 {
		go func() { s, p := send(addrB, "hold"); held <- res{s, p} }()
	}
	waitFor(t, 3*time.Second, "B's two requests in flight", func() bool { return metricL(t, addrB, `llmflow_ha_local_grants{pool="p"}`) == 2 })
	if p := priorityOf(addrB); p < 10 {
		t.Fatalf("a third request of a got priority %d: the reservation of 2 is not shared", p)
	}
	go func() { _, _ = send(addrA, "hold") }() // dies with A
	waitFor(t, 3*time.Second, "A's request in flight", func() bool { return metricL(t, addrA, `llmflow_ha_local_grants{pool="p"}`) == 1 })
	time.Sleep(600 * time.Millisecond) // the owner persisted a's in-flight (3)

	// Traffic on B the whole time.
	var sent, failed atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			st, _ := sendAs(keyB, addrB, "quick")
			sent.Add(1)
			if st != 200 {
				failed.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	killed := time.Now()
	if err := procA.Process.Kill(); err != nil { // SIGKILL, by PID
		t.Fatal(err)
	}
	_ = procA.Wait()
	waitFor(t, 5*time.Second, "B takes the pool over", func() bool { return metricL(t, addrB, `llmflow_ha_owner{pool="p"}`) == 1 })
	took := time.Since(killed)
	epoch2 := metricL(t, addrB, `llmflow_ha_epoch{pool="p"}`)
	if epoch2 <= epoch1 {
		t.Fatalf("epoch %v after the takeover, %v before", epoch2, epoch1)
	}
	// B's two requests are adopted, A's dead one is a phantom: a is still
	// at its reservation.
	if p := priorityOf(addrB); p < 10 {
		t.Fatalf("after the takeover a request of a got priority %d: the dead owner's request was forgotten", p)
	}
	if ph := metricL(t, addrB, `llmflow_inflight_phantoms{pool="p",org="a"}`); ph != 1 {
		t.Fatalf("phantoms of a after the takeover: %v, want 1 (the dead owner's request)", ph)
	}
	close(stop)
	wg.Wait()
	// Release B's held requests: a is back under its reservation once the
	// phantom has expired (max_request_duration).
	mu.Lock()
	close(hold)
	mu.Unlock()
	for range 2 {
		if r := <-held; r.status != 200 || r.prio != 0 {
			t.Fatalf("a held request on B: %d priority %d, want 200 within share", r.status, r.prio)
		}
	}
	waitFor(t, 8*time.Second, "the phantom expires", func() bool {
		return metricL(t, addrB, `llmflow_inflight_phantoms{pool="p",org="a"}`) == 0
	})
	if p := priorityOf(addrB); p != 0 {
		t.Fatalf("a request of a, alone in flight, got priority %d", p)
	}
	t.Logf("takeover %v after SIGKILL, epoch %v -> %v; %d requests on the survivor meanwhile, %d failed, %v admitted without an owner",
		took.Round(time.Millisecond), epoch1, epoch2, sent.Load(), failed.Load(), metric(t, addrB, "llmflow_ha_unowned_total"))
	if failed.Load() != 0 {
		t.Fatalf("%d of %d requests failed on the surviving replica", failed.Load(), sent.Load())
	}
}

// check-config validates the ha block and its secret.
func TestCheckConfigHA(t *testing.T) {
	dir := t.TempDir()
	mk := writeFile(t, dir, "master", "m-secret-0123456789\n")
	sk := writeFile(t, dir, "salt", "0123456789abcdef0123456789abcdef-salt\n")
	dsn := writeFile(t, dir, "dsn", "postgres://x@127.0.0.1/db\n")
	conf := func(secret, peers string) string {
		hk := writeFile(t, dir, "ha", secret+"\n")
		return writeFile(t, dir, "c.hcl", `
master_key_file     = "`+mk+`"
cache_salt_key_file = "`+sk+`"
org "o" {
  share = 1
  team "a" {}
}
pool "p" { capacity = 4 }
model "m" {
  pool = "p"
  upstream "u" {
    base_url = "http://127.0.0.1:1"
    key_file = "`+sk+`"
  }
}
store {
  kind      = "postgres"
  dsn_file  = "`+dsn+`"
  spool_dir = "`+dir+`"
}
ha {
  listen        = "10.1.2.3:9444"
  peer_networks = [`+peers+`]
  secret_file   = "`+hk+`"
}
`)
	}
	if out, err := run(t, "check-config", "-c", conf("0123456789abcdef0123456789abcdef", `"10.1.2.0/24"`)); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := run(t, "check-config", "-c", conf("short", `"10.1.2.0/24"`)); err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("short ha secret: %v", err)
	}
	if _, err := run(t, "check-config", "-c", conf("0123456789abcdef0123456789abcdef", `"10.9.0.0/16"`)); err == nil || !strings.Contains(err.Error(), "outside peer_networks") {
		t.Fatalf("advertise outside peer_networks: %v", err)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	return p
}

func createKeyFor(t *testing.T, addr, team string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://"+addr+"/admin/keys", strings.NewReader(`{"team":"`+team+`"}`))
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

// metricL reads one sample, labels included in name exactly as printed.
func metricL(t *testing.T, addr, name string) float64 {
	t.Helper()
	return metric(t, addr, name)
}

func waitFor(t *testing.T, limit time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, limit)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
