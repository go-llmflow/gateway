package gateway

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Hard refusals for the decision report (#97): a request refused with 429
// because a budget or a workload profile was exhausted is counted per day,
// organisation, team, profile and kind (store.RefusalRow), with the set of the
// people refused (hashes, store.UserHash) so that the report's small-cell rule
// is applied to people: one person refused five times is a cell of one. The
// counts are kept in memory, keyed by their day, and written every
// refusalFlush (and when the gateway stops), so a storm of refusals costs one
// upsert per cell, not one per request; the Prometheus counter
// llmflow_denied_total stays the live view.
//
// The request path only takes a mutex around a map update. A flush swaps the
// pending counts out under the mutex and writes them with the mutex released,
// so a slow database never holds up a refused request; counts that could not
// be written are merged back under their own day.

// refusalFlush is how often the counts are written.
var refusalFlush = 30 * time.Second

// maxRefusalCells bounds the memory a storm can take (cells over all pending
// days): past it a refusal is counted under its kind with the names blanked.
const maxRefusalCells = 4096

type refusalCell struct {
	n     int64
	users map[string]struct{}
}

type refusalCounter struct {
	mu      sync.Mutex
	pending map[string]map[[4]string]*refusalCell // day → cell
	cells   int
}

// refusalKind maps a refused admission to its kind, "" when it is not a hard
// refusal (a budget that is arming, a store failure, ...).
func refusalKind(ae *admitError) string {
	if ae == nil || ae.status != 429 {
		return ""
	}
	switch ae.denyTag {
	case "profile":
		switch ae.key.Kind {
		case store.ScopeProfileSession:
			return store.RefusalProfileSession
		case store.ScopeProfileOverflow:
			return store.RefusalProfileOverflow
		}
		return store.RefusalProfileWeek
	case "budget":
		return store.RefusalBudget
	}
	return ""
}

// add counts n refusals of the people in users in a cell of a day.
func (rc *refusalCounter) add(day string, k [4]string, n int64, users map[string]struct{}) {
	if rc.pending == nil {
		rc.pending = map[string]map[[4]string]*refusalCell{}
	}
	cells := rc.pending[day]
	if cells == nil {
		cells = map[[4]string]*refusalCell{}
		rc.pending[day] = cells
	}
	c := cells[k]
	if c == nil {
		if rc.cells >= maxRefusalCells {
			k = [4]string{"", "", "", k[3]}
			if c = cells[k]; c == nil {
				c = &refusalCell{users: map[string]struct{}{}}
				cells[k] = c
				rc.cells++
			}
		} else {
			c = &refusalCell{users: map[string]struct{}{}}
			cells[k] = c
			rc.cells++
		}
	}
	c.n += n
	for u := range users {
		if len(c.users) >= store.MaxRefusalUsers {
			break
		}
		c.users[u] = struct{}{}
	}
}

// noteRefusal counts one hard refusal of a request of scope.
func (s *Server) noteRefusal(sc reqScope, ae *admitError) {
	kind := refusalKind(ae)
	if kind == "" {
		return
	}
	profile := ""
	if pr, _, ok := s.prof.forUser(sc.user); ok {
		profile = pr.ID
	}
	var users map[string]struct{}
	if sc.user != "" { // a service key is a refusal and no one
		users = map[string]struct{}{store.UserHash(sc.user): {}}
	}
	day := s.now().UTC().Format("2006-01-02")
	rc := &s.refusals
	rc.mu.Lock()
	rc.add(day, [4]string{sc.org, sc.team, profile, kind}, 1, users)
	rc.mu.Unlock()
}

// flushRefusals writes what is pending. The mutex is held only to swap the
// pending counts out and, on a failed write, to merge them back.
func (s *Server) flushRefusals(ctx context.Context) {
	qs, ok := s.st.(store.QuotaReportStore)
	if !ok {
		return
	}
	rc := &s.refusals
	rc.mu.Lock()
	taken := rc.pending
	rc.pending, rc.cells = nil, 0
	rc.mu.Unlock()
	if len(taken) == 0 {
		return
	}
	days := make([]string, 0, len(taken))
	for d := range taken {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, d := range days {
		cells := taken[d]
		day, err := time.Parse("2006-01-02", d)
		if err != nil {
			continue
		}
		rows := make([]store.RefusalRow, 0, len(cells))
		for k, c := range cells {
			us := make([]string, 0, len(c.users))
			for u := range c.users {
				us = append(us, u)
			}
			sort.Strings(us)
			rows = append(rows, store.RefusalRow{Org: k[0], Team: k[1], Profile: k[2], Kind: k[3], N: c.n, Users: us})
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = qs.AddRefusals(wctx, day, rows)
		cancel()
		if err != nil {
			s.log.Error("refusal counts: storing failed; kept for the next time", "day", d, "err", err)
			rc.mu.Lock()
			for k, c := range cells {
				rc.add(d, k, c.n, c.users)
			}
			rc.mu.Unlock()
		}
	}
}

func (s *Server) startRefusals(ctx context.Context) {
	if _, ok := s.st.(store.QuotaReportStore); !ok {
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		t := time.NewTicker(refusalFlush)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.flushRefusals(ctx)
			}
		}
	}()
}
