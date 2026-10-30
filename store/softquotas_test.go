package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/internal/warnat"
)

func TestWarnMemory(t *testing.T) { contractWarn(t, NewMemory()) }

// contractWarn is the soft-quota contract, run on both stores.
func contractWarn(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	wk1 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) // a Monday
	wk2 := wk1.AddDate(0, 0, 7)
	mark := func(user, win string, th int, inst time.Time, rolling bool) bool {
		t.Helper()
		ok, err := s.MarkWarning(ctx, WarnMark{User: user, Window: win, Threshold: th, Instance: inst, Rolling: rolling}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	// A calendar window fires once per instance, then again in the next.
	if !mark("wa", "week", 75, wk1, false) || mark("wa", "week", 75, wk1, false) {
		t.Fatal("week 75: once per instance")
	}
	if !mark("wa", "week", 95, wk1, false) {
		t.Fatal("another threshold is its own mark")
	}
	if mark("wa", "week", 75, wk1.AddDate(0, 0, -7), false) {
		t.Fatal("an earlier instance fired")
	}
	if !mark("wa", "week", 75, wk2, false) || mark("wa", "week", 75, wk2, false) {
		t.Fatal("next week: once")
	}
	if !mark("wb", "week", 75, wk2, false) || !mark("wa", "budget:user:wa::", 75, wk2, false) {
		t.Fatal("another person, another window")
	}
	// A rolling window: fires, then only after being armed again, which needs
	// usage below the threshold minus the hysteresis.
	b0 := BucketOf(time.Date(2026, 10, 28, 12, 3, 0, 0, time.UTC))
	if !mark("ra", "session", 75, b0, true) || mark("ra", "session", 75, b0, true) {
		t.Fatal("session 75: once")
	}
	if mark("ra", "session", 75, b0.Add(BucketWidth), true) {
		t.Fatal("a later bucket fired while the threshold was not re-armed")
	}
	for _, pct := range []float64{99, 75, 70.5, 70} {
		if n, err := s.RearmWarnings(ctx, "ra", "session", pct); err != nil || n != 0 {
			t.Fatalf("armed again at %v%%: %d %v", pct, n, err)
		}
	}
	if n, err := s.RearmWarnings(ctx, "ra", "session", 69.9); err != nil || n != 1 {
		t.Fatalf("not armed again below 70: %d %v", n, err)
	}
	if n, _ := s.RearmWarnings(ctx, "ra", "session", 0); n != 0 {
		t.Fatal("armed twice")
	}
	if !mark("ra", "session", 75, b0.Add(2*BucketWidth), true) || mark("ra", "session", 75, b0.Add(3*BucketWidth), true) {
		t.Fatal("after re-arming: once")
	}
	// Re-arming concerns the person's own window only.
	if n, _ := s.RearmWarnings(ctx, "rb", "session", 0); n != 0 {
		t.Fatal("re-armed another person")
	}
	// A calendar mark is never re-armed by usage.
	if n, _ := s.RearmWarnings(ctx, "wa", "week", 0); n != 0 || mark("wa", "week", 75, wk2, false) {
		t.Fatal("a calendar window re-armed")
	}
	for _, bad := range []WarnMark{{}, {User: "u", Window: "w", Threshold: 0}, {User: "u", Window: "w", Threshold: 100}, {User: "u\n", Window: "w", Threshold: 50}, {User: "u", Window: "", Threshold: 50}} {
		if _, err := s.MarkWarning(ctx, bad, nil); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}

	// Runtime overrides.
	if err := s.SetWarnLevels(ctx, WarnScopeOrg, "etabA", []int{80, 95}, "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWarnLevels(ctx, WarnScopeProfile, "2d", []int{}, "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWarnLevels(ctx, WarnScopeGateway, "", []int{60}, "root"); err != nil {
		t.Fatal(err)
	}
	ls, err := s.WarnLevels(ctx)
	if err != nil || len(ls) != 3 || ls[0].Scope != "gateway" || !reflect.DeepEqual(ls[0].WarnAt, []int{60}) ||
		ls[1].Scope != "org" || ls[1].Name != "etabA" || !reflect.DeepEqual(ls[1].WarnAt, []int{80, 95}) || ls[1].SetBy != "root" ||
		ls[2].Scope != "profile" || ls[2].WarnAt == nil || len(ls[2].WarnAt) != 0 {
		t.Fatalf("%+v %v", ls, err)
	}
	if err := s.SetWarnLevels(ctx, WarnScopeOrg, "etabA", nil, "root"); err != nil {
		t.Fatal(err)
	}
	if ls, _ := s.WarnLevels(ctx); len(ls) != 2 {
		t.Fatalf("not removed: %+v", ls)
	}
	for _, c := range []struct {
		scope, name string
		l           []int
	}{{"nope", "x", []int{50}}, {WarnScopeOrg, "", []int{50}}, {WarnScopeGateway, "x", []int{50}}, {WarnScopeOrg, "o", []int{0}}, {WarnScopeOrg, "o", []int{95, 75}}, {WarnScopeProfile, "p\n", []int{50}}} {
		if err := s.SetWarnLevels(ctx, c.scope, c.name, c.l, "root"); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}

	// warn_at on a user budget round-trips; nil, a list and the empty list.
	for i, want := range [][]int{nil, {80}, {}, {50, 75, 95}} {
		b := Budget{BudgetKey: BudgetKey{Kind: ScopeUser, Name: "wbud"}, Period: Day, MaxCost: float64(10 + i), WarnAt: want}
		if err := s.SetBudget(ctx, b); err != nil {
			t.Fatal(err)
		}
		bs, err := s.Budgets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, g := range bs {
			if g.Kind == ScopeUser && g.Name == "wbud" {
				found = true
				if (g.WarnAt == nil) != (want == nil) || len(g.WarnAt) != len(want) || want != nil && !reflect.DeepEqual(g.WarnAt, want) {
					t.Fatalf("warn_at %v read back as %#v", want, g.WarnAt)
				}
			}
		}
		if !found {
			t.Fatal("budget lost")
		}
	}
	for name, b := range map[string]Budget{
		"not a user budget": {BudgetKey: BudgetKey{Kind: ScopeTeam, Name: "t"}, Period: Day, MaxCost: 1, WarnAt: []int{80}},
		"decreasing":        {BudgetKey: BudgetKey{Kind: ScopeUser, Name: "u"}, Period: Day, MaxCost: 1, WarnAt: []int{95, 80}},
		"out of range":      {BudgetKey: BudgetKey{Kind: ScopeUser, Name: "u"}, Period: Day, MaxCost: 1, WarnAt: []int{100}},
		"too many":          {BudgetKey: BudgetKey{Kind: ScopeUser, Name: "u"}, Period: Day, MaxCost: 1, WarnAt: []int{1, 2, 3, 4, 5, 6}},
	} {
		if err := s.SetBudget(ctx, b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWarnMemoryFault(t *testing.T) {
	m := NewMemory()
	boom := errors.New("boom")
	m.SetFail(boom)
	ctx := context.Background()
	w := WarnMark{User: "u", Window: "week", Threshold: 75, Instance: time.Now()}
	if _, err := m.MarkWarning(ctx, w, nil); !errors.Is(err, boom) {
		t.Fatal("MarkWarning fault")
	}
	if _, err := m.RearmWarnings(ctx, "u", "w", 0); !errors.Is(err, boom) {
		t.Fatal("RearmWarnings fault")
	}
	if _, err := m.WarnLevels(ctx); !errors.Is(err, boom) {
		t.Fatal("WarnLevels fault")
	}
	if err := m.SetWarnLevels(ctx, WarnScopeGateway, "", []int{50}, "x"); !errors.Is(err, boom) {
		t.Fatal("SetWarnLevels fault")
	}
}

// FuzzMarkValidate: no mark panics a store, and one it accepts is in bounds.
func FuzzMarkValidate(f *testing.F) {
	f.Add("u", "week", 75)
	f.Add("", "", 0)
	f.Add("a\nb", "w\x00", 100)
	f.Fuzz(func(t *testing.T, user, win string, th int) {
		w := WarnMark{User: user, Window: win, Threshold: th, Instance: time.Unix(0, 0)}
		ok, err := NewMemory().MarkWarning(context.Background(), w, nil)
		if err == nil && (!ok || th < warnat.Min || th > warnat.Max || user == "") {
			t.Fatalf("accepted %+v: %v", w, ok)
		}
	})
}

func pgForWarn(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	p, err := OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if _, err := p.pool.Exec(context.Background(), `DELETE FROM llmflow_warn_marks; DELETE FROM llmflow_warn_levels`); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWarnPostgres(t *testing.T) { contractWarn(t, pgForWarn(t)) }

// The SQL's threshold - LEAST(5, threshold/2) is warnat.RearmBelow: at the
// bound a threshold is not re-armed, half a point below it is.
func TestRearmSQLMatchesGo(t *testing.T) {
	p := pgForWarn(t)
	ctx := context.Background()
	at := BucketOf(time.Now())
	for th := warnat.Min; th <= warnat.Max; th++ {
		user := fmt.Sprintf("rs%d", th)
		if ok, err := p.MarkWarning(ctx, WarnMark{User: user, Window: "session", Threshold: th, Instance: at, Rolling: true}, nil); !ok || err != nil {
			t.Fatal(ok, err)
		}
		b := float64(warnat.RearmBelow(th))
		if n, err := p.RearmWarnings(ctx, user, "session", b); err != nil || n != 0 {
			t.Fatalf("threshold %d re-armed at its bound %v: %d %v", th, b, n, err)
		}
		if n, err := p.RearmWarnings(ctx, user, "session", b-0.5); err != nil || n != 1 {
			t.Fatalf("threshold %d not re-armed below %v: %d %v", th, b, n, err)
		}
	}
}

// Many replicas see the same crossing at once: exactly one fires, and exactly
// one set of outbox rows is queued, in the same transaction.
func TestWarnPostgresConcurrentOnce(t *testing.T) {
	p := pgForWarn(t)
	ctx := context.Background()
	if _, err := p.pool.Exec(ctx, `DELETE FROM llmflow_alert_outbox WHERE key LIKE 'wq-%'`); err != nil {
		t.Fatal(err)
	}
	wk := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	var fired atomic.Int64
	var wg sync.WaitGroup
	for r := 0; r < 16; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := []WarnOutbox{{Key: "wq-1", Channel: "webhook", Payload: []byte(`{}`)}, {Key: fmt.Sprintf("wq-m-%d", r), Channel: "smtp", Payload: []byte(`{}`)}}
			ok, err := p.MarkWarning(ctx, WarnMark{User: "conc", Window: "week", Threshold: 95, Instance: wk}, out)
			if err != nil {
				t.Error(err)
			}
			if ok {
				fired.Add(1)
			}
		}()
	}
	wg.Wait()
	if fired.Load() != 1 {
		t.Fatalf("fired %d times", fired.Load())
	}
	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM llmflow_alert_outbox WHERE key LIKE 'wq-%'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d outbox rows queued, want the winner's 2: %v", n, err)
	}
}
