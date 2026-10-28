package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Arming (v0.17.1, the follow-up of #87). A budget created over a scope in
// use must also count the requests admitted before this process knew it:
// they hold nothing on it, yet their cost lands in it when their records
// are stored. So this process keeps, for EVERY admitted request, budget or
// not, its worst case W and its scope until its record is stored (or until
// it ends without one). On first seeing a budget that is arming, it reports
// the sum of those W in the budget's scope (store.ReportArming), and lowers
// the report as they settle. The store makes no grant on the budget before
// every registered process has reported, and counts the reports against
// the limit until they are all back to 0.
//
// Ordering: a request is tracked BEFORE it reads the budgets it falls under
// (admitBudget), and a budget is snapshotted AFTER the refresh that
// installs it. A request that missed the budget (no hold) was therefore
// tracked before the snapshot, and is in the report.

// tracked is one admitted request (or fallback candidate) not yet settled.
type tracked struct {
	keys []store.BudgetKey
	w    int64
	at   time.Time
}

// armState is one arming budget as this process reports it: the requests
// it counted (only those, so the report only goes down), and what it last
// reported.
type armState struct {
	period   store.Period
	ids      map[string]bool
	reported int64 // -1: not yet
}

type tracker struct {
	mu      sync.Mutex
	pending map[string][]tracked // by record id
	arming  map[store.BudgetKey]*armState
	wake    chan struct{}
}

func newTracker() *tracker {
	return &tracker{pending: map[string][]tracked{}, arming: map[store.BudgetKey]*armState{}, wake: make(chan struct{}, 1)}
}

// track adds a request (several entries for one id add up: a fallback
// candidate counts until the request ends).
func (t *tracker) track(id string, keys []store.BudgetKey, w int64, at time.Time) {
	if id == "" || w <= 0 {
		return
	}
	t.mu.Lock()
	t.pending[id] = append(t.pending[id], tracked{keys: keys, w: w, at: at})
	t.mu.Unlock()
}

// untrack ends a request: its record is stored, dropped or never written.
func (t *tracker) untrack(ids ...string) {
	t.mu.Lock()
	changed := false
	for _, id := range ids {
		if _, ok := t.pending[id]; ok {
			delete(t.pending, id)
			changed = true
		}
	}
	armed := len(t.arming) > 0
	t.mu.Unlock()
	if changed && armed {
		t.poke()
	}
}

func (t *tracker) poke() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// sync follows the budgets of a refresh: it snapshots the newly arming
// ones and forgets the active ones.
func (t *tracker) sync(bs []store.Budget) {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[store.BudgetKey]bool{}
	for _, b := range bs {
		if !b.Arming {
			continue
		}
		seen[b.BudgetKey] = true
		if t.arming[b.BudgetKey] != nil {
			continue
		}
		st := &armState{period: b.Period, ids: map[string]bool{}, reported: -1}
		for id, ts := range t.pending {
			for _, x := range ts {
				if inKeys(b.BudgetKey, x.keys) {
					st.ids[id] = true
				}
			}
		}
		t.arming[b.BudgetKey] = st
	}
	for k := range t.arming {
		if !seen[k] {
			delete(t.arming, k)
		}
	}
	if len(t.arming) > 0 {
		t.poke()
	}
}

func inKeys(k store.BudgetKey, keys []store.BudgetKey) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

// report is one budget's report to send.
type report struct {
	key store.BudgetKey
	pre int64
}

// due lists the reports that changed: the W still unsettled among the
// requests counted, admitted in the budget's current period (an older
// request is charged to a period the budget did not exist in).
func (t *tracker) due(now time.Time) []report {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []report
	for k, st := range t.arming {
		start := st.period.Start(now)
		var pre int64
		for id := range st.ids {
			ts, ok := t.pending[id]
			if !ok {
				delete(st.ids, id)
				continue
			}
			for _, x := range ts {
				if inKeys(k, x.keys) && !x.at.Before(start) {
					pre = min(pre+x.w, maxLedgerNano)
				}
			}
		}
		if pre != st.reported {
			out = append(out, report{k, pre})
		}
	}
	return out
}

func (t *tracker) reported(k store.BudgetKey, pre int64, active bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if st := t.arming[k]; st != nil {
		st.reported = pre
		if active {
			delete(t.arming, k)
		}
	}
}

// armingLoop sends the reports when they change (at once on a settlement,
// and at least every second while a budget is arming).
func (s *Server) armingLoop(ctx context.Context) {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		case <-s.tracker.wake:
		}
		s.sendReports(ctx)
	}
}

func (s *Server) sendReports(ctx context.Context) {
	for _, r := range s.tracker.due(s.now()) {
		rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		active, err := s.st.ReportArming(rctx, s.escrow.holder, r.key, r.pre)
		cancel()
		if err != nil {
			s.log.Error("arming report failed", "budget", r.key.String(), "err", err)
			continue
		}
		s.tracker.reported(r.key, r.pre, active)
		if active {
			s.escrow.forgetRoom()
		}
	}
}
