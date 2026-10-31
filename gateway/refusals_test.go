package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

func refusalFixtureServer(t *testing.T) *env {
	t.Helper()
	up := newFakeVLLM(t)
	return newEnv(t, testConfig(up.URL()))
}

// A failed write keeps the counts under THEIR day: yesterday's refusals are
// never written as today's.
func TestRefusalsKeepTheirDayOnFailure(t *testing.T) {
	e := refusalFixtureServer(t)
	rc := &e.srv.refusals
	today := e.srv.now().UTC().Format("2006-01-02")
	yesterday := e.srv.now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	cell := [4]string{"etabA", "t1", "", store.RefusalBudget}
	rc.mu.Lock()
	rc.add(yesterday, cell, 4, map[string]struct{}{store.UserHash("alice"): {}})
	rc.mu.Unlock()
	e.srv.noteRefusal(reqScope{org: "etabA", team: "t1", user: "bob"}, &admitError{status: 429, denyTag: "budget"})

	e.st.SetFail(errors.New("database down"))
	e.srv.flushRefusals(context.Background())
	e.st.SetFail(nil)
	rc.mu.Lock()
	if len(rc.pending) != 2 || rc.pending[yesterday][cell] == nil || rc.pending[yesterday][cell].n != 4 || rc.pending[today][cell] == nil || rc.pending[today][cell].n != 1 {
		t.Fatalf("pending after a failed flush: %+v", rc.pending)
	}
	rc.mu.Unlock()

	e.srv.flushRefusals(context.Background())
	d0, _ := time.Parse("2006-01-02", yesterday)
	rows, err := e.st.RefusalsBetween(context.Background(), d0, d0.AddDate(0, 0, 1), 10)
	if err != nil || len(rows) != 1 || rows[0].N != 4 {
		t.Fatalf("yesterday: %+v %v", rows, err)
	}
	rows, err = e.st.RefusalsBetween(context.Background(), d0.AddDate(0, 0, 1), d0.AddDate(0, 0, 2), 10)
	if err != nil || len(rows) != 1 || rows[0].N != 1 {
		t.Fatalf("today: %+v %v", rows, err)
	}
	users, err := e.st.RefusalUsersBetween(context.Background(), d0, d0.AddDate(0, 0, 1), 10)
	if err != nil || len(users) != 1 || users[0].User != store.UserHash("alice") {
		t.Fatalf("yesterday's people: %+v %v", users, err)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.pending) != 0 || rc.cells != 0 {
		t.Fatalf("pending after a good flush: %+v", rc.pending)
	}
}

// slowRefusals is a store whose refusal write waits: the request path must not.
type slowRefusals struct {
	*store.Memory
	entered chan struct{}
	release chan struct{}
}

func (s *slowRefusals) AddRefusals(ctx context.Context, day time.Time, rows []store.RefusalRow) error {
	close(s.entered)
	<-s.release
	return s.Memory.AddRefusals(ctx, day, rows)
}

// A flush in a slow database does not hold up a refused request.
func TestRefusalsDoNotBlockBehindTheDatabase(t *testing.T) {
	e := refusalFixtureServer(t)
	slow := &slowRefusals{Memory: e.st, entered: make(chan struct{}), release: make(chan struct{})}
	e.srv.st = slow
	e.srv.noteRefusal(reqScope{org: "etabA", team: "t1", user: "bob"}, &admitError{status: 429, denyTag: "budget"})
	done := make(chan struct{})
	go func() { e.srv.flushRefusals(context.Background()); close(done) }()
	<-slow.entered // the write is in progress, the mutex must be free
	returned := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			e.srv.noteRefusal(reqScope{org: "etabA", team: "t1", user: "carol"}, &admitError{status: 429, denyTag: "budget"})
		}
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("a refused request waits for the database")
	}
	close(slow.release)
	<-done
	e.srv.flushRefusals(context.Background())
	today := e.srv.now().UTC().Truncate(24 * time.Hour)
	rows, err := e.st.RefusalsBetween(context.Background(), today, today.AddDate(0, 0, 1), 10)
	var n int64
	for _, r := range rows {
		n += r.N
	}
	if err != nil || n != 101 {
		t.Fatalf("%d refusals written, want 101: %+v %v", n, rows, err)
	}
	e.srv.st = slow.Memory
}
