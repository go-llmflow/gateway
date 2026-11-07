package gateway

import (
	"fmt"
	"testing"
)

// The fair-share estimate of a request's tokens (what its team's level counts
// while it runs) was prompt + max_tokens, whatever n or best_of asked for: a
// request of n=8 generates up to 8 completions, so a team could hold eight
// times the work the share level showed (audit 2026-11-07, traced; proven
// here from the pool's in-flight token count).
func TestFairShareCountsEveryChoiceOfARequest(t *testing.T) {
	inflight := func(e *env, team string) int64 {
		for _, s := range e.srv.groups["llama"].pool.snapshot() {
			if s.Team == team {
				return s.InFlightTokens
			}
		}
		return 0
	}
	const body = `{"model":"llama","max_tokens":10,%s"messages":[{"role":"user","content":"hello"}]}`
	var base int64
	for _, tc := range []struct {
		name, extra string
		choices     int64
	}{
		{"one choice", ``, 1},
		{"n = 4", `"n":4,`, 4},
		{"best_of = 6", `"best_of":6,`, 6},
		{"n = 3, best_of = 5", `"n":3,"best_of":5,`, 5},
	} {
		up := newFakeVLLM(t)
		e := newEnv(t, testConfig(up.URL()))
		_, key := e.newKey("etabA")
		up.gate = make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			readAll(t, e.do("POST", "/v1/chat/completions", key, fmt.Sprintf(body, tc.extra)))
		}()
		waitUpstream(t, up, 1)
		got := inflight(e, "etabA")
		close(up.gate)
		<-done
		if tc.choices == 1 {
			base = got
			continue
		}
		// prompt tokens are the same; the completion estimate is max_tokens per choice.
		// (the prompt estimate grows by a byte or two with the extra field: at least.)
		if want := base + 10*(tc.choices-1); got < want {
			t.Errorf("%s: %d tokens in flight, want at least %d (the one-choice estimate %d plus 10 per further choice)", tc.name, got, want, base)
		}
	}
}
