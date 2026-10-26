package gateway

import (
	"context"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/fairshare"
)

// Slurm discovery with several replicas (#75 with #74), on a real
// PostgreSQL: every replica discovers the instances for its own routing;
// the pool's owner decides C and the reservations with its own view and
// publishes them with the epoch.

// slurmHAConfig is the Slurm test configuration reading slurmrestd at
// restd, with stale_after replaced.
func slurmHAConfig(t *testing.T, restd string, staleAfter time.Duration) *config.Config {
	t.Helper()
	c := slurmConfig(t, restd)
	groups := append([]config.ModelGroup(nil), c.ModelGroups...)
	for i := range groups {
		if groups[i].Slurm != nil {
			sl := *groups[i].Slurm
			sl.StaleAfter = staleAfter
			groups[i].Slurm = &sl
		}
	}
	c.ModelGroups = groups
	return c
}

// overReservation watches every replica's decided C and reservations
// until stop, and reports the first time their sum exceeded C.
type overReservation struct {
	mu      sync.Mutex
	bad     string
	samples int
	stop    chan struct{}
	done    sync.WaitGroup
}

func watchReservations(envs ...*env) *overReservation {
	o := &overReservation{stop: make(chan struct{})}
	o.done.Add(1)
	go func() {
		defer o.done.Done()
		for {
			select {
			case <-o.stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			for _, e := range envs {
				_, epoch := e.srv.ha.Status("gpu")
				c := e.srv.ha.Capacity("gpu")
				sum := 0
				for _, r := range e.srv.ha.Reserved("gpu") {
					sum += r
				}
				o.mu.Lock()
				o.samples++
				if sum > c && o.bad == "" {
					o.bad = e.srv.ha.Instance() + " at epoch " + itoa(epoch) + ": reservations " + itoa(int64(sum)) + " > C " + itoa(int64(c))
				}
				o.mu.Unlock()
			}
		}
	}()
	return o
}

func (o *overReservation) check(t *testing.T) {
	t.Helper()
	close(o.stop)
	o.done.Wait()
	if o.bad != "" {
		t.Fatalf("over-reservation: %s", o.bad)
	}
	t.Logf("%d samples, the sum of reservations never above C", o.samples)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func haOwnerOf(e *env, pool string) bool { o, _ := e.srv.ha.Status(pool); return o }

// crash cuts a replica off as a SIGKILL would: no lease released, no
// usage written, its internal port gone, PostgreSQL out of reach.
func crash(e *env, px *chaosProxy) {
	px.set("refuse")
	e.gw.CloseClientConnections()
	e.gw.Close()
	_ = e.srv.haServer.Close()
}

func proxiedDB(t *testing.T) (direct, viaProxy string, px *chaosProxy) {
	t.Helper()
	au, err := url.Parse(pgAdminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	px = newChaosProxy(t, au.Host)
	direct, viaProxy = freshDB(t, px.addr())
	return direct, viaProxy, px
}

// The owner dies while a vLLM job joins: the survivor, which discovered
// the new instance itself, takes the pool with a higher epoch and decides
// with the new C; until it owns the pool requests go at the lowest
// priority; the reservations never exceed C.
func TestChaosHASlurmOwnerKilledWhileJoining(t *testing.T) {
	restd := newFakeSlurmrestd(t)
	in1, in2 := newSlurmInstance(t, "meta/llama"), newSlurmInstance(t, "meta/llama")
	restd.set(slurmJobOf(101, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", in1))
	cfg := slurmHAConfig(t, restd.srv.URL, time.Hour)
	direct, viaProxy, px := proxiedDB(t)
	a := haEnv(t, cfg, viaProxy, "a")
	eventually(t, 3*time.Second, "a owns the pool once it knows the members", func() bool { return haOwnerOf(a, "gpu") })
	b := haEnv(t, cfg, direct, "b")
	eventually(t, 3*time.Second, "b knows the published C", func() bool { return b.srv.ha.Capacity("gpu") == 2 })
	watch := watchReservations(a, b)
	_, e1 := b.srv.ha.Status("gpu")

	// A second instance joins; b discovers it, a dies before it does.
	restd.set(slurmJobOf(101, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", in1),
		slurmJobOf(102, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", in2))
	crash(a, px)
	b.srv.groups["llama"].disc.poll(context.Background())
	if g, ok, _ := b.srv.ha.Acquire("gpu", "etabA", fairshare.Interactive, 1, false); ok && !haOwnerOf(b, "gpu") {
		if g.Priority != fairshare.BorrowBase+fairshare.RankStep*fairshare.MaxRank {
			t.Fatalf("before the takeover a request got priority %d, want the lowest", g.Priority)
		}
		g.Release(0)
	}
	eventually(t, 5*time.Second, "b takes the pool over", func() bool { return haOwnerOf(b, "gpu") })
	_, e2 := b.srv.ha.Status("gpu")
	if e2 <= e1 {
		t.Fatalf("epoch %d after the takeover, was %d", e2, e1)
	}
	if c := b.srv.ha.Capacity("gpu"); c != 4 {
		t.Fatalf("the new owner decides with C = %d, want 4 (both instances, its own view)", c)
	}
	eventually(t, 2*time.Second, "C published with the new epoch", func() bool {
		l := b.srv.ha.Published("gpu")
		return l.Epoch == e2 && l.Capacity == 4
	})
	watch.check(t)
}

// slurmrestd is unreachable for the owner only: it yields the pool to the
// replica that still reads the jobs, which keeps quiet for a lease before
// deciding, so the two never decide at once.
func TestChaosHASlurmRestdLostByOwner(t *testing.T) {
	restdA, restdB := newFakeSlurmrestd(t), newFakeSlurmrestd(t)
	in1 := newSlurmInstance(t, "meta/llama")
	job := slurmJobOf(101, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", in1)
	restdA.set(job)
	restdB.set(job)
	direct, _ := freshDB(t, "")
	a := haEnv(t, slurmHAConfig(t, restdA.srv.URL, time.Millisecond), direct, "a")
	eventually(t, 3*time.Second, "a owns the pool", func() bool { return haOwnerOf(a, "gpu") })
	b := haEnv(t, slurmHAConfig(t, restdB.srv.URL, time.Millisecond), direct, "b")
	eventually(t, 3*time.Second, "b knows the members", func() bool { r, _ := b.srv.poolMembership("gpu"); return r })
	watch := watchReservations(a, b)
	var both, sampled int
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			sampled++
			if haOwnerOf(a, "gpu") && haOwnerOf(b, "gpu") {
				both++
			}
		}
	}()
	_, e1 := a.srv.ha.Status("gpu")
	restdA.setDown(true)
	a.srv.groups["llama"].disc.poll(context.Background()) // stale at once (stale_after 1ms)
	if _, fresh := a.srv.poolMembership("gpu"); fresh {
		t.Fatal("the owner still believes its membership fresh")
	}
	cut := time.Now()
	eventually(t, 5*time.Second, "b owns the pool", func() bool { return haOwnerOf(b, "gpu") })
	took := time.Since(cut)
	if haOwnerOf(a, "gpu") {
		t.Fatal("the yielding owner still decides")
	}
	_, e2 := b.srv.ha.Status("gpu")
	restdA.setDown(false)
	a.srv.groups["llama"].disc.poll(context.Background())
	time.Sleep(time.Second)
	if haOwnerOf(a, "gpu") {
		t.Fatal("a took the pool back from a live owner")
	}
	close(stop)
	wg.Wait()
	watch.check(t)
	t.Logf("handover %v after slurmrestd was lost by the owner, epoch %d -> %d; %d samples, %d with two owners", took.Round(time.Millisecond), e1, e2, sampled, both)
	if both != 0 || e2 <= e1 {
		t.Fatalf("two owners in %d samples, epoch %d -> %d", both, e1, e2)
	}
	if took < 900*time.Millisecond {
		t.Fatalf("the new owner decided %v after the handover, before a whole lease (900ms) had passed", took)
	}
}

// Two replicas disagree on the members: the owner sees two instances, the
// other one (its slurmrestd lists one). Each routes to what it sees; the
// fair share is the owner's, also for the requests the other one forwards.
func TestChaosHASlurmDisagree(t *testing.T) {
	restdA, restdB := newFakeSlurmrestd(t), newFakeSlurmrestd(t)
	in1, in2 := newSlurmInstance(t, "meta/llama"), newSlurmInstance(t, "meta/llama")
	j1 := slurmJobOf(101, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", in1)
	j2 := slurmJobOf(102, "llmflow:llama:PORT", "svc_llm", "gpu", "svc_llm", in2)
	restdA.set(j1, j2)
	restdB.set(j1)
	direct, _ := freshDB(t, "")
	a := haEnv(t, slurmHAConfig(t, restdA.srv.URL, time.Hour), direct, "a")
	eventually(t, 3*time.Second, "a owns the pool", func() bool { return haOwnerOf(a, "gpu") })
	b := haEnv(t, slurmHAConfig(t, restdB.srv.URL, time.Hour), direct, "b")
	watch := watchReservations(a, b)
	eventually(t, 3*time.Second, "b reads the owner's C", func() bool { return b.srv.ha.Capacity("gpu") == 4 })
	eventually(t, 3*time.Second, "b knows its members", func() bool { r, _ := b.srv.poolMembership("gpu"); return r })
	if na, nb := len(a.srv.groups["llama"].members()), len(b.srv.groups["llama"].members()); na != 2 || nb != 1 {
		t.Fatalf("routing members: a %d, b %d; want each its own view (2, 1)", na, nb)
	}
	// etabA's reservation is the owner's (0.75 x 4 = 3), not b's (0.75 x 2).
	var grants []*fairshare.Grant
	for i := range 4 {
		g, ok, err := b.srv.ha.Acquire("gpu", "etabA", fairshare.Interactive, 1, false)
		if !ok || err != nil {
			t.Fatal(ok, err)
		}
		if want := i >= 3; g.Borrowing != want {
			t.Fatalf("request %d through b: borrowing %v, want %v (the owner's reservation of 3)", i+1, g.Borrowing, want)
		}
		grants = append(grants, g)
	}
	for _, g := range grants {
		g.Release(0)
	}
	watch.check(t)
}
