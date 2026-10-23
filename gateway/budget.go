package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Budget errors.
var (
	errBudgetExceeded = errors.New("budget exhausted for this period")
	errStateStale     = errors.New("accounting store unreachable; requests are refused (fail-closed)")
)

// budgeter keeps every team's spend for its current budget period in
// memory, refreshed from the store periodically, so that the request path
// never waits on PostgreSQL. Between refreshes it adds the cost of the
// requests this process finished.
type budgeter struct {
	mu         sync.Mutex
	st         store.Store
	now        func() time.Time
	budgets    map[string]store.Budget
	dbSpend    map[string]float64
	local      map[string]float64 // cost added since the last refresh began
	lastOK     time.Time
	everOK     bool
	maxAge     time.Duration
	failClosed bool
}

func newBudgeter(st store.Store, refresh time.Duration, failClosed bool, now func() time.Time) *budgeter {
	return &budgeter{
		st: st, now: now, budgets: map[string]store.Budget{}, dbSpend: map[string]float64{},
		local: map[string]float64{}, maxAge: 3 * refresh, failClosed: failClosed,
	}
}

// refresh reloads budgets and spend. Records still in the accounting queue
// when the query runs are counted by neither side until the next refresh:
// the spend can lag by at most one queue drain.
func (b *budgeter) refresh(ctx context.Context) error {
	b.mu.Lock()
	snapshot := b.local
	b.local = map[string]float64{}
	b.mu.Unlock()

	restore := func() {
		b.mu.Lock()
		for k, v := range snapshot {
			b.local[k] += v
		}
		b.mu.Unlock()
	}
	list, err := b.st.Budgets(ctx)
	if err != nil {
		restore()
		return err
	}
	now := b.now()
	starts := map[time.Time]bool{}
	for _, bg := range list {
		starts[bg.Period.Start(now)] = true
	}
	spendAt := map[time.Time]map[string]float64{}
	for s := range starts {
		m, err := b.st.SpendSince(ctx, s)
		if err != nil {
			restore()
			return err
		}
		spendAt[s] = m
	}
	budgets := map[string]store.Budget{}
	spend := map[string]float64{}
	for _, bg := range list {
		budgets[bg.Team] = bg
		spend[bg.Team] = spendAt[bg.Period.Start(now)][bg.Team]
	}
	b.mu.Lock()
	b.budgets, b.dbSpend, b.lastOK, b.everOK = budgets, spend, now, true
	b.mu.Unlock()
	return nil
}

func (b *budgeter) loop(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := b.refresh(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// check reports whether a team may start a request.
func (b *budgeter) check(team string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failClosed && (!b.everOK || b.now().Sub(b.lastOK) > b.maxAge) {
		return errStateStale
	}
	bg, ok := b.budgets[team]
	if !ok {
		return nil
	}
	if b.dbSpend[team]+b.local[team] >= bg.MaxCost {
		return errBudgetExceeded
	}
	return nil
}

func (b *budgeter) add(team string, cost float64) {
	if cost <= 0 {
		return
	}
	b.mu.Lock()
	b.local[team] += cost
	b.mu.Unlock()
}

// healthy reports whether the last refresh is recent.
func (b *budgeter) healthy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.everOK && b.now().Sub(b.lastOK) <= b.maxAge
}
