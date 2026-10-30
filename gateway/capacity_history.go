package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// The history of every pool's C (#97, #108). The lending report integrates a
// pool's guaranteed slots over time, and a pool fed by Slurm jobs has a C that
// changes with its instances (recomputeCapacity audits each change as
// pool.capacity). The audit log says what changed; the report needs it as a
// series it can read in one query: one row per change, written when the
// change is applied and checked every minute for what a replica not owning
// the pool is told by the owner (ha).

type capacityNotes struct {
	mu   sync.Mutex
	last map[string]int
}

// capacityTick is how often C is looked at, besides every change.
var capacityTick = time.Minute

// noteCapacity records a pool's C when it differs from the last one recorded.
func (s *Server) noteCapacity(pool string) {
	cs, ok := s.st.(store.CapacityStore)
	if !ok {
		return
	}
	c := s.poolCapacity(pool)
	s.capNotes.mu.Lock()
	if s.capNotes.last == nil {
		s.capNotes.last = map[string]int{}
	}
	prev, seen := s.capNotes.last[pool]
	s.capNotes.last[pool] = c
	s.capNotes.mu.Unlock()
	if seen && prev == c {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cs.RecordCapacity(ctx, store.CapacityPoint{Pool: pool, At: s.now().UTC().Truncate(time.Millisecond), C: c}); err != nil {
		s.log.Error("pool capacity history: storing a point failed", "pool", pool, "err", err)
		s.capNotes.mu.Lock()
		delete(s.capNotes.last, pool) // try again at the next look
		s.capNotes.mu.Unlock()
	}
}

func (s *Server) startCapacityHistory(ctx context.Context) {
	if _, ok := s.st.(store.CapacityStore); !ok {
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		t := time.NewTicker(capacityTick)
		defer t.Stop()
		for {
			for _, p := range s.cfg.Pools {
				s.noteCapacity(p.Name)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}
