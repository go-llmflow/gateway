package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
)

// fakeSlurmrestd answers GET /slurm/v0.0.44/jobs/ with the jobs set by the
// test, the shape of OPENAPI_JOB_INFO_RESP (data_parser v0.0.44).
type fakeSlurmrestd struct {
	srv  *httptest.Server
	mu   sync.Mutex
	jobs []map[string]any
	down bool
	hits int
	// badToken counts requests without the expected X-SLURM-USER-TOKEN.
	badToken int
}

func newFakeSlurmrestd(t *testing.T) *fakeSlurmrestd {
	f := &fakeSlurmrestd{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hits++
		if r.Header.Get("X-SLURM-USER-TOKEN") != internalKey {
			f.badToken++
			http.Error(w, `{"errors":[{"error":"auth"}]}`, http.StatusUnauthorized)
			return
		}
		if f.down {
			http.Error(w, "slurmctld unreachable", http.StatusBadGateway)
			return
		}
		if r.URL.Path != "/slurm/v0.0.44/jobs/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": f.jobs, "last_update": time.Now().Unix(), "errors": []any{}, "warnings": []any{}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlurmrestd) set(jobs ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append([]map[string]any{}, jobs...) // [] and not null, as slurmrestd answers
}

func (f *fakeSlurmrestd) setDown(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = v
}

// slurmJobOf builds a JOB_INFO of a vLLM instance listening at inst.
func slurmJobOf(id int, name, account, partition, user string, inst *slurmInstance, state ...string) map[string]any {
	if len(state) == 0 {
		state = []string{"RUNNING"}
	}
	u, _ := url.Parse(inst.srv.URL)
	return map[string]any{"job_id": id, "name": strings.ReplaceAll(name, "PORT", u.Port()), "comment": "", "account": account,
		"partition": partition, "user_name": user, "batch_host": u.Hostname(), "nodes": u.Hostname(), "job_state": state}
}

// slurmInstance is a vLLM instance started as a job: /health (503 when
// down), /v1/models (behind the key) and the fake vLLM for the rest.
type slurmInstance struct {
	srv   *httptest.Server
	vllm  *fakeVLLM
	down  atomic.Bool
	model string
}

func newSlurmInstance(t *testing.T, model string) *slurmInstance {
	in := &slurmInstance{vllm: newFakeVLLM(t), model: model}
	in.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case in.down.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.URL.Path == "/health":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/models":
			if r.Header.Get("Authorization") != "Bearer "+internalKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model"}]}`, in.model)
		default:
			in.vllm.serve(w, r)
		}
	}))
	t.Cleanup(in.srv.Close)
	return in
}

func slurmConfig(t *testing.T, restd string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(`
master_key_file = "/unused"
cache_salt_key_file = "/unused"
budget_refresh  = "1h"
key_cache_ttl   = "1h"
health {
  interval = "1h"
  cooldown = "1h"
}
org "etabA" {
  share = 0.75
  team "etabA" {}
}
org "etabB" {
  share = 0.25
  team "etabB" {}
}
pool "gpu" { capacity = 0 }
model "llama" {
  upstream_model = "meta/llama"
  pool           = "gpu"
  key_file       = "/keys/vllm"
  slurm {
    url                   = "`+restd+`"
    token_file            = "/keys/slurm-jwt"
    accounts              = ["svc_llm"]
    partitions            = ["gpu"]
    users                 = ["svc_llm"]
    interval              = "1h"
    stale_after           = "2h"
    instance_capacity     = 2
    max_instance_capacity = 8
  }
}
`), "slurm.hcl")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The pool follows the trusted RUNNING jobs: added once healthy and
// serving the model, drained and removed when the job ends or keeps
// failing health; C and the reservations follow; foreign jobs never get
// in; a slurmrestd outage keeps the healthy members.
func TestSlurmDiscovery(t *testing.T) {
	restd := newFakeSlurmrestd(t)
	clk := &clock{t: time.Now()}
	e := newEnv(t, slurmConfig(t, restd.srv.URL), WithClock(clk.now))
	g := e.srv.groups["llama"]
	d := g.disc
	pool := e.srv.pools["gpu"]
	ctx := context.Background()
	_, key := e.newKey("etabA")
	members := func() int { return len(g.members()) }

	d.poll(ctx)
	if pool.Capacity() != 0 || members() != 0 {
		t.Fatalf("no job: C %d, %d members", pool.Capacity(), members())
	}
	expect(t, "no instance", e.chatWith(key, chat), http.StatusBadGateway)

	a := newSlurmInstance(t, "meta/llama")
	b := newSlurmInstance(t, "meta/llama")
	wrong := newSlurmInstance(t, "other/model")
	fake := newSlurmInstance(t, "meta/llama")
	restd.set(
		slurmJobOf(10, "llmflow:llama:PORT:3", "svc_llm", "gpu", "svc_llm", a),
		slurmJobOf(11, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", b),
		slurmJobOf(12, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", wrong),
		// Untrusted: another account, another partition, another user, a
		// job not yet running, a capacity above the bound.
		slurmJobOf(20, "llmflow:llama:PORT", "physics", "gpu", "alice", fake),
		slurmJobOf(21, "llmflow:llama:PORT", "svc_llm", "debug", "svc_llm", fake),
		slurmJobOf(22, "llmflow:llama:PORT", "svc_llm", "gpu", "alice", fake),
		slurmJobOf(23, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", fake, "PENDING"),
		slurmJobOf(24, "llmflow:llama:PORT:999", "svc_llm", "gpu", "svc_llm", fake),
		slurmJobOf(25, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", fake, "RUNNING", "COMPLETING"),
	)
	d.poll(ctx)
	if pool.Capacity() != 5 || members() != 2 {
		t.Fatalf("2 trusted instances (3 + default 2): C %d, %d members", pool.Capacity(), members())
	}
	if r := pool.Reserved(); r["etabA"]+r["etabB"] != 5 || r["etabA"] != 4 {
		t.Fatalf("reservations at C=5 (0.75/0.25): %v", r)
	}
	for i := 0; i < 4; i++ {
		expect(t, "through the discovered instances", e.chatWith(key, chat), 200)
	}
	if a.vllm.count()+b.vllm.count() != 4 || fake.vllm.count() != 0 || wrong.vllm.count() != 0 {
		t.Fatalf("requests: a %d b %d wrong %d fake %d", a.vllm.count(), b.vllm.count(), wrong.vllm.count(), fake.vllm.count())
	}
	served := a // pick takes the least loaded, first in URL order: either may have them all
	if a.vllm.count() == 0 {
		served = b
	}
	if got := served.vllm.last().Header.Get("Authorization"); got != "Bearer "+internalKey {
		t.Errorf("the instance got %q, not the model's key", got)
	}
	metrics := readAll(t, e.do("GET", "/metrics", "", ""))
	for _, want := range []string{`llmflow_pool_capacity{pool="gpu"} 5`, `llmflow_slurm_rejected_jobs_total{model="llama",reason="account"} 1`,
		`reason="partition"} 1`, `reason="user"} 1`, `reason="capacity"} 1`, `llmflow_slurm_polls_total{model="llama",outcome="ok"}`} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	// slurmrestd down: the members stay, even past stale_after; nothing new
	// is admitted (job 13 is invisible).
	c := newSlurmInstance(t, "meta/llama")
	restd.setDown(true)
	d.poll(ctx)
	clk.add(3 * time.Hour)
	d.poll(ctx)
	if pool.Capacity() != 5 || members() != 2 {
		t.Fatalf("outage: C %d, %d members", pool.Capacity(), members())
	}
	expect(t, "during the outage", e.chatWith(key, chat), 200)
	restd.setDown(false)

	// Job 10 ends, job 13 starts: one drained and removed, one added.
	restd.set(slurmJobOf(11, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", b), slurmJobOf(13, "llmflow:llama:PORT:4", "svc_llm", "gpu", "svc_llm", c))
	d.poll(ctx)
	if pool.Capacity() != 6 || members() != 2 {
		t.Fatalf("after job 10 ended and 13 started: C %d, %d members", pool.Capacity(), members())
	}

	// b fails health: drained after 3 polls, then removed.
	b.down.Store(true)
	d.poll(ctx)
	d.poll(ctx)
	if pool.Capacity() != 6 {
		t.Fatalf("2 failed probes already drained: C %d", pool.Capacity())
	}
	d.poll(ctx)
	if pool.Capacity() != 4 || members() != 1 {
		t.Fatalf("after 3 failed probes: C %d, %d members", pool.Capacity(), members())
	}
	nb := b.vllm.count()
	expect(t, "after the drain", e.chatWith(key, chat), 200)
	if b.vllm.count() != nb {
		t.Error("a drained instance got a request")
	}

	// The changes are audited.
	e.srv.Close(ctx)
	entries, err := e.st.AuditRange(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, en := range entries {
		seen[en.Action]++
	}
	if seen["slurm.member_added"] != 3 || seen["slurm.member_drained"] != 2 || seen["slurm.member_removed"] != 2 || seen["pool.capacity"] != 3 {
		t.Errorf("audit: %v", seen)
	}
	if restd.badToken != 0 {
		t.Errorf("%d slurmrestd requests without the token", restd.badToken)
	}
}

// A draining instance keeps its requests in flight until they finish.
func TestSlurmDrainWaitsForInflight(t *testing.T) {
	restd := newFakeSlurmrestd(t)
	e := newEnv(t, slurmConfig(t, restd.srv.URL))
	g := e.srv.groups["llama"]
	a := newSlurmInstance(t, "meta/llama")
	restd.set(slurmJobOf(10, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", a))
	ctx := context.Background()
	g.disc.poll(ctx)
	u := g.members()[0]
	u.inflight.Add(1) // a request still running there
	restd.set()
	g.disc.poll(ctx)
	if len(g.members()) != 1 || !u.draining.Load() || e.srv.pools["gpu"].Capacity() != 0 {
		t.Fatalf("draining with a request in flight: %d members, draining %v, C %d", len(g.members()), u.draining.Load(), e.srv.pools["gpu"].Capacity())
	}
	if g.pick(map[*upstream]bool{}, time.Now()) != nil {
		t.Fatal("a draining instance was picked")
	}
	u.inflight.Add(-1)
	g.disc.poll(ctx)
	if len(g.members()) != 0 {
		t.Fatal("not removed once idle")
	}
}

// slurmrestd's answer is untrusted input: errors, oversize, a redirect
// (which would carry the JWT elsewhere) and garbage are all failures.
func TestSlurmFetchRefuses(t *testing.T) {
	var mode atomic.Value
	elsewhere := 0
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere++ }))
	defer other.Close()
	restd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load().(string) {
		case "errors":
			fmt.Fprint(w, `{"jobs":[],"errors":[{"error":"Invalid authentication","description":"token expired"}]}`)
		case "redirect":
			http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
		case "garbage":
			fmt.Fprint(w, `{"jobs":[{"job_id":"x"`)
		case "big":
			fmt.Fprint(w, `{"jobs":[],"pad":"`+strings.Repeat("a", maxSlurmBody)+`"}`)
		}
	}))
	defer restd.Close()
	e := newEnv(t, slurmConfig(t, restd.URL))
	d := e.srv.groups["llama"].disc
	for _, m := range []string{"errors", "redirect", "garbage", "big"} {
		mode.Store(m)
		if _, err := d.fetch(context.Background()); err == nil {
			t.Errorf("%s: accepted", m)
		}
	}
	if elsewhere != 0 {
		t.Error("the redirect was followed")
	}
}

// Whatever a job's name and batch_host say, an accepted instance is
// http://<that host>:<a valid port>: nothing in a job can steer the URL
// elsewhere.
func FuzzSlurmQualify(f *testing.F) {
	for _, s := range [][2]string{{"llmflow:llama:8000", "gpu01"}, {"llmflow:llama:8000:4", "gpu01.cluster"},
		{"llmflow:llama:80@evil:1", "h"}, {"llmflow:llama:8000", "evil.com/x?"}, {"llmflow:llama:08000", "h"},
		{"llmflow:llama:8000", "user@evil"}, {"llmflow:llama:8000", "[::1]"}, {"llmflow:llama:65536", "h"}} {
		f.Add(s[0], s[1])
	}
	d := &slurmDiscovery{s: &Server{log: slog.New(slog.DiscardHandler), met: newMetrics()}, g: &group{cfg: config.ModelGroup{Name: "llama"}},
		cfg:     config.Slurm{Accounts: []string{"svc"}, Partitions: []string{"gpu"}, Scheme: "http", InstanceCapacity: 1, MaxInstanceCapacity: 8},
		ignored: map[string]bool{}}
	f.Fuzz(func(t *testing.T, name, host string) {
		c, ok := d.qualify(slurmJob{JobID: 1, Name: name, Account: "svc", Partition: "gpu", BatchHost: host, JobState: json.RawMessage(`["RUNNING"]`)})
		if !ok {
			return
		}
		u, err := url.Parse(c.base)
		if err != nil || u.Hostname() != host || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" {
			t.Fatalf("name %q host %q gave %q", name, host, c.base)
		}
		if c.capacity < 1 || c.capacity > 8 {
			t.Fatalf("capacity %d", c.capacity)
		}
	})
}

func TestSlurmConfig(t *testing.T) {
	ok := `
master_key_file = "/unused"
cache_salt_key_file = "/unused"
budget_refresh  = "1h"
org "o" {
  share = 1
  team "t" {}
}
pool "p" { capacity = 0 }
model "m" {
  pool     = "p"
  key_file = "/k"
  slurm {
    url        = "https://slurm.example:6820"
    token_file = "/jwt"
    accounts   = ["svc_llm"]
    partitions = ["gpu"]
  }
}
`
	c, err := config.Parse([]byte(ok), "s.hcl")
	if err != nil {
		t.Fatal(err)
	}
	s := c.ModelGroups[0].Slurm
	if s.APIVersion != "v0.0.44" || s.Interval != 15*time.Second || s.StaleAfter != 5*time.Minute || s.InstanceCapacity != 1 || s.Scheme != "http" {
		t.Errorf("defaults %+v", s)
	}
	if kf := strings.Join(c.KeyFiles(), ","); kf != "/k,/jwt" {
		t.Errorf("key files %s", kf)
	}
	for name, bad := range map[string][2]string{
		"no account":       {`accounts   = ["svc_llm"]`, `accounts = []`},
		"bad account":      {`"svc_llm"`, `"svc llm"`},
		"url path":         {`:6820"`, `:6820/slurm"`},
		"url credentials":  {`https://slurm`, `https://u:p@slurm`},
		"api version":      {`token_file = "/jwt"`, "token_file = \"/jwt\"\n    api_version = \"latest\""},
		"capacity bounds":  {`token_file = "/jwt"`, "token_file = \"/jwt\"\n    instance_capacity = 9\n    max_instance_capacity = 4"},
		"no key file":      {`key_file = "/k"`, ``},
		"stale < interval": {`token_file = "/jwt"`, "token_file = \"/jwt\"\n    stale_after = \"1s\""},
	} {
		if _, err := config.Parse([]byte(strings.Replace(ok, bad[0], bad[1], 1)), "s.hcl"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// capacity = 0 only for a pool fed by Slurm.
	static := ok[:strings.Index(ok, "  slurm {")] + `  upstream "u" { base_url = "http://h:1" }` + "\n}\n"
	if _, err := config.Parse([]byte(static), "s.hcl"); err == nil || !strings.Contains(err.Error(), "capacity = 0") {
		t.Errorf("a static pool of capacity 0: %v", err)
	}
}
