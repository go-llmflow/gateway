package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
	"github.com/go-llmflow/gateway/store"
)

type escrowFixture struct {
	st   *store.Memory
	e    *escrow
	now  time.Time
	mu   sync.Mutex
	key  store.BudgetKey
	refs []budgetRef
}

func newEscrowFixture(t testing.TB, limit float64) *escrowFixture {
	f := &escrowFixture{st: store.NewMemory(), now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), key: store.BudgetKey{Kind: store.ScopeTeam, Name: "t"}}
	clock := func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
	f.e = newEscrow(f.st, store.Holder{Replica: "r", Incarnation: "i"}, 0.5, time.Second, time.Minute, clock)
	f.setLimit(t, limit)
	return f
}

func (f *escrowFixture) setLimit(t testing.TB, limit float64) {
	t.Helper()
	if err := f.st.SetBudgetChecked(context.Background(), store.Budget{BudgetKey: f.key, Period: store.Day, MaxCost: limit}, f.now, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	bs, _ := f.st.Budgets(context.Background())
	f.refs = []budgetRef{{key: f.key, start: store.Day.Start(f.now), epoch: bs[0].Epoch, period: store.Day}}
}

func (f *escrowFixture) ledger() store.LedgerState {
	st, _ := f.st.Ledger(context.Background(), f.key, store.Day.Start(f.now))
	return st
}

// A grant grows by a share of the room left, admissions within it touch
// nothing else, and an idle one goes back.
func TestEscrowGrowsAndReturns(t *testing.T) {
	f := newEscrowFixture(t, 10)
	ctx := context.Background()
	h, err := f.e.acquire(ctx, f.refs, nanoPerUnit)
	if err != nil {
		t.Fatal(err)
	}
	// max(1, 0.5 × 10) = 5 units granted at once.
	if st := f.ledger(); st.Granted != 5*nanoPerUnit || f.e.refills.Load() != 1 {
		t.Fatalf("first growth: %+v, %d refills", st, f.e.refills.Load())
	}
	for i := 0; i < 4; i++ {
		if _, err := f.e.acquire(ctx, f.refs, nanoPerUnit); err != nil {
			t.Fatal(err)
		}
	}
	if f.e.refills.Load() != 1 {
		t.Fatalf("admissions within the grant went to the store: %d refills", f.e.refills.Load())
	}
	h.settle(nanoPerUnit / 2) // half of W used: the rest stays this process's
	if _, err := f.e.acquire(ctx, f.refs, nanoPerUnit/2); err != nil || f.e.refills.Load() != 1 {
		t.Fatalf("the leftover was not lent again: %v, %d refills", err, f.e.refills.Load())
	}
	// Idle: everything unused goes back.
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Second)
	f.mu.Unlock()
	f.e.tick(ctx, true)
	// Nothing unused: 4.5 units in flight, and the store still holds the
	// finished request's W until its record settles it.
	if st := f.ledger(); st.Granted != 5*nanoPerUnit {
		t.Fatalf("after the idle tick: %+v", st)
	}
	rec := store.Record{ID: "r1", Time: f.now, AdmittedAt: f.now, Team: "t", CostNano: nanoPerUnit / 2, Cost: 0.5,
		HoldReplica: "r", HoldIncarnation: "i", HoldNano: nanoPerUnit, Holds: []store.HoldRef{{Key: f.key, Start: store.Day.Start(f.now)}}}
	if err := f.st.InsertRecords(ctx, []store.Record{rec}); err != nil {
		t.Fatal(err)
	}
	if st := f.ledger(); st.Granted != 5*nanoPerUnit-nanoPerUnit/2 || st.Spent != nanoPerUnit/2 {
		t.Fatalf("after the settlement: %+v", st)
	}
}

// What does not fit is refused with the room left, near the limit grants
// shrink to exactly W, and a refusal is not asked again at once.
func TestEscrowNearTheLimit(t *testing.T) {
	f := newEscrowFixture(t, 3)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := f.e.acquire(ctx, f.refs, nanoPerUnit); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	_, err := f.e.acquire(ctx, f.refs, 1)
	var nr *noRoom
	if !errors.As(err, &nr) || nr.room != 0 {
		t.Fatalf("over the limit: %v", err)
	}
	if st := f.ledger(); st.Granted != 3*nanoPerUnit {
		t.Fatalf("granted %+v", st)
	}
	refills := f.e.refills.Load()
	if _, err := f.e.acquire(ctx, f.refs, 1); err == nil || f.e.refills.Load() != refills {
		t.Fatal("a cached refusal went to the store")
	}
}

// A new epoch (a forced lower limit) stops lending what was granted; what
// a request admitted before leaves unused goes back when it finishes.
func TestEscrowEpoch(t *testing.T) {
	f := newEscrowFixture(t, 10)
	ctx := context.Background()
	h, err := f.e.acquire(ctx, f.refs, nanoPerUnit)
	if err != nil {
		t.Fatal(err)
	}
	f.setLimit(t, 1) // forced: 5 granted > 1, 1 of it in flight
	if _, err := f.e.acquire(ctx, f.refs, nanoPerUnit); err == nil {
		t.Fatal("lent a grant made under the old limit")
	}
	if st := f.ledger(); st.Granted != nanoPerUnit {
		t.Fatalf("the unused grant did not go back: %+v", st)
	}
	h.settle(nanoPerUnit / 4)
	f.e.tick(ctx, false)
	if st := f.ledger(); st.Granted != 0 && st.Granted != nanoPerUnit/4 {
		t.Fatalf("after the epoch: %+v", st)
	}
	f.e.mu.Lock()
	c := f.e.cells[cellKey{f.key, store.Day.Start(f.now).UnixNano()}]
	granted, admitted := c.granted, c.admitted
	f.e.mu.Unlock()
	if admitted != 0 || granted != 0 {
		t.Fatalf("cell after the request: granted %d admitted %d", granted, admitted)
	}
}

// A grant that relies on a temporary increase is not lent past its end.
func TestEscrowTemporaryIncrease(t *testing.T) {
	f := newEscrowFixture(t, 1)
	ctx := context.Background()
	if err := f.st.IncreaseBudgetChecked(ctx, f.key, 10, f.now.Add(10*time.Minute), f.now, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	bs, _ := f.st.Budgets(ctx)
	f.refs[0].epoch = bs[0].Epoch
	if _, err := f.e.acquire(ctx, f.refs, 2*nanoPerUnit); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.now = f.now.Add(9*time.Minute + time.Second) // past expiry - lending time
	f.mu.Unlock()
	if _, err := f.e.acquire(ctx, f.refs, nanoPerUnit/2); err == nil {
		t.Fatal("lent a grant past the end of the increase it relied on")
	}
}

// BenchmarkAdmission: the admission of one request (W, hold, release) with
// no budget, within a local grant (no store access), and with a grant
// refill every time (a store transaction; the memory store here). Run with
// -benchtime=1x in CI; measure at load ≤ 4, interleaved, one core.
func BenchmarkAdmission(b *testing.B) {
	up := newFakeVLLM(b)
	cfg := testConfig(up.URL())
	cfg.ModelGroups[0].MaxModelLen = 4096
	// Workload profiles (#90): a large one, and one whose week is always
	// full so that every admission goes to the overflow cap.
	cfg.Profiles = config.Profiles{Workday: map[string]float64{"": 1000},
		List:          []config.Profile{{ID: "big", Days: 7, SessionShare: 1}, {ID: "full", Days: 0, SessionShare: 1, OverflowCap: 200}},
		SessionWindow: 5 * time.Hour}
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		b.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer srv.Close(context.Background())
	g := srv.groups["llama"]
	body := []byte(`{"model":"meta/llama","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	ctx := context.Background()
	run := func(b *testing.B, team string, user ...string) {
		scope := reqScope{org: "o", team: team, key: "k", model: "llama"}
		if len(user) > 0 {
			scope.user = user[0]
		}
		b.ReportAllocs()
		for b.Loop() {
			adm, _, err := srv.admitBudget(ctx, openai.Chat, g, scope, body, nil, "bench")
			if err != nil {
				b.Fatal(err)
			}
			adm.release(0)
			srv.tracker.untrack("bench")
		}
	}
	b.Run("no-budget", func(b *testing.B) { run(b, "free") })
	set := func(team string, limit float64) {
		if err := st.SetBudget(ctx, store.Budget{BudgetKey: store.BudgetKey{Kind: store.ScopeTeam, Name: team}, Period: store.Day, MaxCost: limit}); err != nil {
			b.Fatal(err)
		}
		if err := srv.budget.refresh(ctx); err != nil {
			b.Fatal(err)
		}
	}
	set("rich", 1e9)
	b.Run("local-grant", func(b *testing.B) { run(b, "rich") })
	// With a profile: its weekly and session budgets held too; and beyond
	// the week, the refusal then the overflow cap, every time.
	for user, prof := range map[string]string{"bench-big": "big", "bench-full": "full"} {
		if err := st.AssignProfile(ctx, store.Assignment{User: user, Profile: prof, Overflow: prof == "full", From: time.Now()}); err != nil {
			b.Fatal(err)
		}
	}
	if err := srv.reconcileProfiles(ctx); err != nil {
		b.Fatal(err)
	}
	b.Run("profile-local-grant", func(b *testing.B) { run(b, "rich", "bench-big") })
	b.Run("profile-overflow", func(b *testing.B) { run(b, "rich", "bench-full") })
	// Every admission refills: the grant is given back after each one.
	b.Run("grant-refill", func(b *testing.B) {
		scope := reqScope{org: "o", team: "rich", key: "k", model: "llama"}
		for b.Loop() {
			adm, _, err := srv.admitBudget(ctx, openai.Chat, g, scope, body, nil, "bench")
			if err != nil {
				b.Fatal(err)
			}
			adm.release(0)
			srv.tracker.untrack("bench")
			srv.escrow.tick(ctx, true)
		}
	})
}
