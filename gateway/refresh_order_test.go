package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// staleStore answers its first Budgets call with the list it read, then waits
// before returning it: a refresh that read the store before a budget was
// created and installs its answer after the one that read it afterwards.
type staleStore struct {
	store.Store
	first   atomic.Bool
	entered chan struct{}
	gate    chan struct{}
}

func (s *staleStore) Budgets(ctx context.Context) ([]store.Budget, error) {
	bs, err := s.Store.Budgets(ctx)
	if s.first.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.gate
	}
	return bs, err
}

// Two refreshes at once install in the order they read the store: a budget
// created between them is not made to disappear by the older answer, so the
// arming snapshot (tracker.sync) is taken once, with the requests that were
// in flight when the budget appeared, not again later with those admitted
// meanwhile (which hold on the budget already). Before the fix the budget
// was missing from the final state and TestBudgetArmsOverARunningRequest
// refused a request the budget had room for.
func TestRefreshesInstallInTheOrderTheyRead(t *testing.T) {
	mem := store.NewMemory()
	ss := &staleStore{Store: mem, entered: make(chan struct{}), gate: make(chan struct{})}
	b := newBudgeter(ss, time.Hour, true, time.Now)
	tr := newTracker()
	b.onBudgets = tr.sync
	ctx := context.Background()

	aDone := make(chan error, 1)
	go func() { aDone <- b.refresh(ctx) }() // reads the store without the budget, then waits
	<-ss.entered

	k := store.BudgetKey{Kind: store.ScopeTeam, Name: "etabA"}
	if err := mem.SetBudget(ctx, store.Budget{BudgetKey: k, Period: store.Day, MaxCost: 0.3}); err != nil {
		t.Fatal(err)
	}
	tr.track("first", []store.BudgetKey{k}, 208_000_000, time.Now())
	bDone := make(chan error, 1)
	go func() { bDone <- b.refresh(ctx) }() // reads it after the budget exists

	select {
	case <-bDone:
		t.Fatal("the second refresh finished while the first, which read earlier, was still running: they can install out of order")
	case <-time.After(100 * time.Millisecond):
	}
	close(ss.gate)
	if err := <-aDone; err != nil {
		t.Fatal(err)
	}
	if err := <-bDone; err != nil {
		t.Fatal(err)
	}
	if !b.has(k) {
		t.Fatal("the budget was lost: the older answer was installed last")
	}
	// A request admitted after the budget was seen is not in its snapshot.
	tr.track("second", []store.BudgetKey{k}, 208_000_000, time.Now())
	if err := b.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	reps := tr.due(time.Now())
	if len(reps) != 1 || reps[0].pre != 208_000_000 {
		t.Fatalf("report %+v: want the one request in flight when the budget appeared", reps)
	}
}
