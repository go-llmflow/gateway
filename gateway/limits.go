package gateway

import (
	"sync"
	"time"
)

// window is a sliding one-minute counter approximated from two fixed
// windows (the previous minute weighted by the part of it that still
// overlaps the last 60 s), the usual O(1) rate-limit estimator.
type window struct {
	start     time.Time
	cur, prev int64
}

func (w *window) roll(now time.Time) {
	m := now.Truncate(time.Minute)
	switch {
	case w.start.Equal(m):
	case w.start.Add(time.Minute).Equal(m):
		w.prev, w.cur, w.start = w.cur, 0, m
	default:
		w.prev, w.cur, w.start = 0, 0, m
	}
}

func (w *window) value(now time.Time) float64 {
	w.roll(now)
	elapsed := now.Sub(w.start).Seconds() / 60
	return float64(w.prev)*(1-elapsed) + float64(w.cur)
}

type keyState struct {
	rpm      window
	tpm      window
	inflight int
}

// limiter enforces per-key RPM, TPM and max in-flight. State is local to
// the process (see README, open defects: several replicas multiply the
// limits).
type limiter struct {
	mu   sync.Mutex
	keys map[string]*keyState
	now  func() time.Time
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{keys: map[string]*keyState{}, now: now}
}

// limitError says which limit refused a request.
type limitError string

func (e limitError) Error() string { return string(e) }

// admit checks and records one request; release must follow when admitted.
func (l *limiter) admit(id string, rpm, tpm, maxInflight int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	s := l.keys[id]
	if s == nil {
		s = &keyState{}
		l.keys[id] = s
	}
	if maxInflight > 0 && s.inflight >= maxInflight {
		return limitError("max in-flight requests reached for this key")
	}
	if rpm > 0 && s.rpm.value(now)+1 > float64(rpm) {
		return limitError("requests per minute limit reached for this key")
	}
	if tpm > 0 && s.tpm.value(now) >= float64(tpm) {
		return limitError("tokens per minute limit reached for this key")
	}
	s.rpm.cur++
	s.inflight++
	return nil
}

// release ends an admitted request and charges its tokens.
func (l *limiter) release(id string, tokens int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.keys[id]
	if s == nil {
		return
	}
	s.inflight--
	if tokens > 0 {
		s.tpm.roll(l.now())
		s.tpm.cur += tokens
	}
}
