package ha

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/fairshare"
	"github.com/go-llmflow/gateway/store"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

type member struct {
	t    *testing.T
	n    *Node
	srv  *http.Server
	ln   net.Listener
	view *View
	dead atomic.Bool
}

func testConfig(inst, addr string) Config {
	return Config{
		Instance: inst, Advertise: addr, Secret: testSecret,
		PeerNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		LeaseTTL:     600 * time.Millisecond, Heartbeat: 100 * time.Millisecond, ForwardTimeout: 300 * time.Millisecond,
		UsagePersist: 150 * time.Millisecond, MaxRequest: 2 * time.Second,
		Pools: []PoolSpec{{Name: "p", Capacity: 4, Shares: map[string]float64{"a": 0.5, "b": 0.5}, HalfLife: time.Hour}},
	}
}

func startMember(t *testing.T, st store.HAStore, inst string, mut func(*Config)) *member {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(inst, ln.Addr().String())
	if mut != nil {
		mut(&cfg)
	}
	view := NewView(st)
	n, err := New(cfg, view)
	if err != nil {
		t.Fatal(err)
	}
	m := &member{t: t, n: n, ln: ln, view: view, srv: &http.Server{Handler: n.Handler(), ReadHeaderTimeout: time.Second}}
	go func() { _ = m.srv.Serve(ln) }()
	n.Start(context.Background())
	t.Cleanup(m.kill)
	return m
}

// kill stops a member without any goodbye: no usage written, no lease
// released, its listener gone (a crash, as SIGKILL leaves it).
func (m *member) kill() {
	if !m.dead.CompareAndSwap(false, true) {
		return
	}
	_ = m.srv.Close()
	m.n.stop()
}

func eventually(t *testing.T, limit time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func acquire(t *testing.T, m *member, org string) *fairshare.Grant {
	t.Helper()
	g, ok, err := m.n.Acquire("p", org, fairshare.Interactive, 100, false)
	if err != nil || !ok {
		t.Fatalf("acquire on %s: ok=%v err=%v", m.n.cfg.Instance, ok, err)
	}
	return g
}

func owner(m *member) bool { o, _ := m.n.Status("p"); return o }

func TestReplayWindow(t *testing.T) {
	g := newReplayGuard()
	now := time.Now()
	for _, c := range []struct {
		seq  uint64
		want bool
	}{{1, true}, {1, false}, {3, true}, {2, true}, {2, false}, {5000, true}, {3, false}, {905, true}, {905, false}, {904, false}, {906, true}, {0, false}, {5001, true}, {5000, false}} {
		if got := g.accept("x", c.seq, now); got != c.want {
			t.Fatalf("seq %d: accepted=%v, want %v", c.seq, got, c.want)
		}
	}
	// Another instance has its own window.
	if !g.accept("y", 1, now) {
		t.Fatal("instances share a window")
	}
}

// One reservation across replicas: org a owns 2 of 4 slots, whichever
// replica its requests arrive at.
func TestForwardKeepsOneReservation(t *testing.T) {
	st := NewMemStore(nil)
	m1 := startMember(t, st, "r1", nil)
	m2 := startMember(t, st, "r2", nil)
	if !owner(m1) || owner(m2) {
		t.Fatal("the first replica started should own the pool")
	}
	g1 := acquire(t, m2, "a")
	g2 := acquire(t, m1, "a")
	g3 := acquire(t, m2, "a")
	if g1.Borrowing || g2.Borrowing || !g3.Borrowing {
		t.Fatalf("borrowing = %v %v %v, want false false true (one reservation of 2)", g1.Borrowing, g2.Borrowing, g3.Borrowing)
	}
	if m1.n.Degraded() != 0 || m2.n.Degraded() != 0 {
		t.Fatal("degraded admissions with a live owner")
	}
	g1.Release(1)
	eventually(t, time.Second, "release reaches the owner", func() bool {
		for _, s := range m1.n.Snapshot("p") {
			if s.Team == "a" {
				return s.InFlight == 2 && s.Usage > 0.99
			}
		}
		return false
	})
	if g4 := acquire(t, m2, "a"); g4.Borrowing {
		t.Fatal("a slot released on one replica is not available to the other")
	}
}

// The owner dies mid-traffic: another replica takes over with a higher
// epoch, adopts the surviving replica's grants, counts the dead owner's
// requests as phantoms until max_request_duration, and the ranking (usage)
// survives.
func TestOwnerDeathFailover(t *testing.T) {
	st := NewMemStore(nil)
	m1 := startMember(t, st, "r1", nil)
	m2 := startMember(t, st, "r2", nil)
	b := acquire(t, m1, "b")
	b.Release(5) // usage for b, to see it survive
	acquire(t, m1, "a")
	acquire(t, m2, "a")
	acquire(t, m2, "a")
	time.Sleep(400 * time.Millisecond) // usage persisted (every 150 ms)
	_, e1 := m1.n.Status("p")
	m1.kill()
	eventually(t, 2*time.Second, "takeover", func() bool { return owner(m2) })
	_, e2 := m2.n.Status("p")
	if e2 <= e1 {
		t.Fatalf("epoch %d after takeover, was %d", e2, e1)
	}
	var a, bb fairshare.TeamStat
	for _, s := range m2.n.Snapshot("p") {
		switch s.Team {
		case "a":
			a = s
		case "b":
			bb = s
		}
	}
	if a.InFlight != 2 || a.Phantoms != 1 {
		t.Fatalf("a: in flight %d, phantoms %d; want 2 adopted and 1 phantom (the dead owner's)", a.InFlight, a.Phantoms)
	}
	if bb.Usage < 4.9 {
		t.Fatalf("b's usage %v did not survive the failover", bb.Usage)
	}
	if g := acquire(t, m2, "a"); !g.Borrowing {
		t.Fatal("the dead owner's request was forgotten: a third request of a is within share")
	}
	eventually(t, 3*time.Second, "phantom expiry", func() bool {
		for _, s := range m2.n.Snapshot("p") {
			if s.Team == "a" {
				return s.Phantoms == 0
			}
		}
		return false
	})
}

// An owner cut from the database stops deciding before another replica
// can take the lease; the other takes it with a higher epoch.
func TestPartitionedOwnerStopsDeciding(t *testing.T) {
	st := NewMemStore(nil)
	var mu sync.Mutex
	var log []decision
	rec := func(c *Config) {
		c.OnDecide = func(_ string, epoch int64, inst string) {
			mu.Lock()
			log = append(log, decision{epoch, inst, time.Now()})
			mu.Unlock()
		}
	}
	m1 := startMember(t, st, "r1", rec)
	m2 := startMember(t, st, "r2", rec)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, m := range []*member{m1, m2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if g, ok, _ := m.n.Acquire("p", "a", fairshare.Interactive, 1, false); ok {
					g.Release(0.001)
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	m1.view.Cut(true)
	cut := time.Now()
	eventually(t, time.Second, "the partitioned owner stops deciding", func() bool { return !owner(m1) })
	if d := time.Since(cut); d > 600*time.Millisecond {
		t.Fatalf("stopped deciding after %v, longer than the lease", d)
	}
	eventually(t, 2*time.Second, "takeover", func() bool { return owner(m2) })
	m1.view.Cut(false)
	time.Sleep(300 * time.Millisecond)
	if owner(m1) {
		t.Fatal("the healed replica took the lease back while it was held")
	}
	close(stop)
	wg.Wait()
	checkDecisions(t, log)
}

// Two replicas race for an expired lease: exactly one wins, with epoch+1.
func TestLeaseRace(t *testing.T) {
	testLeaseRace(t, NewMemStore(nil))
}

func testLeaseRace(t *testing.T, st store.HAStore) {
	ctx := context.Background()
	for round := range 20 {
		pool := "race" + string(rune('a'+round))
		if _, err := st.ClaimLease(ctx, pool, "old", "127.0.0.1:1", 0, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		var wg sync.WaitGroup
		res := make([]store.Lease, 8)
		for i := range res {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l, err := st.ClaimLease(ctx, pool, "r"+string(rune('0'+i)), "127.0.0.1:2", 0, time.Minute)
				if err != nil {
					t.Error(err)
				}
				res[i] = l
			}()
		}
		wg.Wait()
		winners := map[string]bool{}
		for _, l := range res {
			if l.Epoch != 2 {
				t.Fatalf("round %d: epoch %d, want 2", round, l.Epoch)
			}
			winners[l.Owner] = true
		}
		if len(winners) != 1 {
			t.Fatalf("round %d: %d owners for one epoch: %v", round, len(winners), winners)
		}
	}
}

// A replica that has seen a newer epoch refuses a stale owner's answer,
// and the stale owner steps down when told.
func TestStaleOwnerRefused(t *testing.T) {
	st := NewMemStore(nil)
	m1 := startMember(t, st, "r1", nil)
	m2 := startMember(t, st, "r2", nil)
	_, e := m1.n.Status("p")
	ps := m2.n.pools["p"]
	ps.mu.Lock()
	ps.known = e + 1
	ps.mu.Unlock()
	g := acquire(t, m2, "a")
	if !g.Borrowing || g.Priority != fairshare.BorrowBase+fairshare.RankStep*fairshare.MaxRank {
		t.Fatalf("priority %d: the stale owner's decision was used", g.Priority)
	}
	if owner(m1) {
		t.Fatal("the stale owner kept deciding after a peer showed a newer epoch")
	}
}

// Clock skew of +-2 minutes between replicas: leases are timed by the
// store's clock and validity by each replica's monotonic clock, and
// signatures accept 5 minutes, so nothing changes.
func TestClockSkew(t *testing.T) {
	for _, skew := range []time.Duration{2 * time.Minute, -2 * time.Minute} {
		st := NewMemStore(nil)
		m1 := startMember(t, st, "r1", nil)
		m2 := startMember(t, st, "r2", func(c *Config) { c.Now = func() time.Time { return time.Now().Add(skew) } })
		time.Sleep(700 * time.Millisecond) // more than a lease: renewals happened
		if !owner(m1) || owner(m2) {
			t.Fatalf("skew %v: ownership moved (r1 %v, r2 %v)", skew, owner(m1), owner(m2))
		}
		acquire(t, m2, "a")
		if m2.n.Degraded() != 0 {
			t.Fatalf("skew %v: the skewed replica could not reach the owner", skew)
		}
		m1.kill()
		eventually(t, 2*time.Second, "takeover by the skewed replica", func() bool { return owner(m2) })
		m2.kill()
	}
}

func TestInternalAPIAuthentication(t *testing.T) {
	st := NewMemStore(nil)
	m1 := startMember(t, st, "r1", nil)
	// A request without a signature, and one signed with another secret.
	resp, err := http.Post("http://"+m1.ln.Addr().String()+pathAcquire, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned request: %d", resp.StatusCode)
	}
	cfg := testConfig("evil", "127.0.0.1:9")
	cfg.Secret = []byte("ffffffffffffffffffffffffffffffff")
	evil, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	var out acquireResp
	if err := evil.call(context.Background(), m1.ln.Addr().String(), pathAcquire, acquireReq{Pool: "p", ID: "x", Org: "a"}, &out); err == nil {
		t.Fatal("a request signed with another secret was accepted")
	}
	// An owner address outside peer_networks is never dialled.
	cfg = testConfig("r9", "127.0.0.1:9")
	cfg.PeerNetworks = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	far, _ := New(cfg, st)
	if err := far.call(context.Background(), m1.ln.Addr().String(), pathAcquire, acquireReq{Pool: "p", ID: "x", Org: "a"}, &out); err != errBadPeer {
		t.Fatalf("call outside peer_networks: %v", err)
	}
}

// A replica that stops reporting (dead, or cut from the owner) has its
// grants turned into phantoms, which expire after max_request_duration.
func TestSilentReplicaGrantsBecomePhantoms(t *testing.T) {
	st := NewMemStore(nil)
	m1 := startMember(t, st, "r1", nil)
	m2 := startMember(t, st, "r2", nil)
	acquire(t, m2, "a")
	acquire(t, m2, "a")
	m2.kill()
	eventually(t, time.Second, "silent replica's grants become phantoms", func() bool {
		for _, s := range m1.n.Snapshot("p") {
			if s.Team == "a" {
				return s.InFlight == 0 && s.Phantoms == 2
			}
		}
		return false
	})
	eventually(t, 3*time.Second, "phantoms expire", func() bool {
		for _, s := range m1.n.Snapshot("p") {
			if s.Team == "a" {
				return s.Phantoms == 0
			}
		}
		return false
	})
}

type decision struct {
	epoch int64
	inst  string
	at    time.Time
}

// checkDecisions: one instance per epoch, and the decisions of an epoch
// all come before those of any later epoch.
func checkDecisions(t *testing.T, log []decision) {
	t.Helper()
	if err := decisionsError(log); err != nil {
		t.Fatal(err)
	}
}

func decisionsError(log []decision) error {
	who := map[int64]string{}
	first, last := map[int64]time.Time{}, map[int64]time.Time{}
	for _, d := range log {
		if w, ok := who[d.epoch]; ok && w != d.inst {
			return fmt.Errorf("epoch %d decided by %s and %s", d.epoch, w, d.inst)
		}
		who[d.epoch] = d.inst
		if f, ok := first[d.epoch]; !ok || d.at.Before(f) {
			first[d.epoch] = d.at
		}
		if l := last[d.epoch]; d.at.After(l) {
			last[d.epoch] = d.at
		}
	}
	for e, l := range last {
		for e2, f := range first {
			if e2 > e && !l.Before(f) {
				return fmt.Errorf("epoch %d (%s) decided at %v, after epoch %d (%s) began at %v: two owners at once",
					e, who[e], l.Format(time.StampMicro), e2, who[e2], f.Format(time.StampMicro))
			}
		}
	}
	if len(who) == 0 {
		return errors.New("no decision recorded: the property was not exercised")
	}
	return nil
}

// The checker sees what it looks for: planted violations fail it.
func TestDecisionCheckerSeesViolations(t *testing.T) {
	t0 := time.Now()
	ok := []decision{{1, "a", t0}, {1, "a", t0.Add(1)}, {2, "b", t0.Add(2)}}
	if err := decisionsError(ok); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]decision{
		"two owners, one epoch": {{1, "a", t0}, {1, "b", t0.Add(1)}},
		"overlap":               {{1, "a", t0}, {2, "b", t0.Add(1)}, {1, "a", t0.Add(2)}},
		"nothing":               nil,
	} {
		if decisionsError(bad) == nil {
			t.Fatalf("%s: not detected", name)
		}
	}
}

// Property: under random partitions from the database, crashes and
// restarts of replicas, with traffic on every replica, no two owners
// ever decide at the same epoch, and epochs never overlap in time.
func TestPropertyOneOwnerPerEpoch(t *testing.T) {
	propertyOneOwnerPerEpoch(t, NewMemStore(nil), 1)
}

func propertyOneOwnerPerEpoch(t *testing.T, st store.HAStore, seed uint64) {
	var mu sync.Mutex
	var log []decision
	rec := func(c *Config) {
		c.LeaseTTL, c.Heartbeat = 300*time.Millisecond, 50*time.Millisecond
		c.OnDecide = func(_ string, epoch int64, inst string) {
			mu.Lock()
			log = append(log, decision{epoch, inst, time.Now()})
			mu.Unlock()
		}
	}
	rng := rand.New(rand.NewPCG(seed, 42))
	members := make([]*member, 4)
	gen := 0
	spawn := func(i int) {
		gen++
		members[i] = startMember(t, st, fmt.Sprintf("r%d-%d", i, gen), rec)
	}
	for i := range members {
		spawn(i)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	traffic := func(m *member) {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if m.dead.Load() {
				return
			}
			if g, ok, _ := m.n.Acquire("p", "a", fairshare.Interactive, 1, false); ok {
				g.Release(0.001)
			}
			time.Sleep(500 * time.Microsecond)
		}
	}
	for _, m := range members {
		wg.Add(1)
		go traffic(m)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		i := rng.IntN(len(members))
		if rng.IntN(2) == 0 {
			// Aim at the owner half of the time, so ownership moves.
			for j, m := range members {
				if !m.dead.Load() && owner(m) {
					i = j
				}
			}
		}
		m := members[i]
		switch rng.IntN(4) {
		case 0:
			m.view.Cut(true)
		case 1:
			m.view.Cut(false)
		case 2:
			if rng.IntN(3) == 0 {
				m.kill()
				spawn(i)
				wg.Add(1)
				go traffic(members[i])
			}
		case 3:
			// a pause: nothing
		}
		time.Sleep(time.Duration(50+rng.IntN(400)) * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	epochs := map[int64]bool{}
	for _, d := range log {
		epochs[d.epoch] = true
	}
	t.Logf("%d decisions over %d epochs", len(log), len(epochs))
	if len(epochs) < 3 {
		t.Fatalf("only %d epochs: the chaos did not move ownership", len(epochs))
	}
	checkDecisions(t, log)
}
