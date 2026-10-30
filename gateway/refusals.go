package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Hard refusals for the decision report (#97): a request refused with 429
// because a budget or a workload profile was exhausted is counted per day,
// organisation, team, profile and kind (store.RefusalRow). Never a person.
// The counts are kept in memory and written every refusalFlush (and when the
// gateway stops), so a storm of refusals costs one upsert per cell, not one
// per request; the Prometheus counter llmflow_denied_total stays the live
// view.

// refusalFlush is how often the counts are written.
var refusalFlush = 30 * time.Second

// maxRefusalCells bounds the memory a storm can take: past it a refusal is
// counted under its kind with the names blanked.
const maxRefusalCells = 4096

type refusalCounter struct {
	mu    sync.Mutex
	day   string
	cells map[[4]string]int64
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
	rc := &s.refusals
	rc.mu.Lock()
	defer rc.mu.Unlock()
	day := s.now().UTC().Format("2006-01-02")
	if rc.day != day { // the day moved: what was counted belongs to the day before
		rc.flushLocked(s, context.Background())
		rc.day = day
	}
	if rc.cells == nil {
		rc.cells = map[[4]string]int64{}
	}
	k := [4]string{sc.org, sc.team, profile, kind}
	if _, ok := rc.cells[k]; !ok && len(rc.cells) >= maxRefusalCells {
		k = [4]string{"", "", "", kind}
	}
	rc.cells[k]++
}

// flushLocked writes the counts of rc.day and empties them; a failed write
// keeps them for the next time.
func (rc *refusalCounter) flushLocked(s *Server, ctx context.Context) {
	qs, ok := s.st.(store.QuotaReportStore)
	if !ok || len(rc.cells) == 0 {
		return
	}
	day, err := time.Parse("2006-01-02", rc.day)
	if err != nil {
		return
	}
	rows := make([]store.RefusalRow, 0, len(rc.cells))
	for k, n := range rc.cells {
		rows = append(rows, store.RefusalRow{Org: k[0], Team: k[1], Profile: k[2], Kind: k[3], N: n})
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := qs.AddRefusals(ctx, day, rows); err != nil {
		s.log.Error("refusal counts: storing failed; kept for the next time", "err", err)
		return
	}
	rc.cells = map[[4]string]int64{}
}

func (s *Server) flushRefusals(ctx context.Context) {
	s.refusals.mu.Lock()
	defer s.refusals.mu.Unlock()
	s.refusals.flushLocked(s, ctx)
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
