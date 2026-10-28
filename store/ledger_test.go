package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

const unitNano = 1_000_000_000

// contractLedger is the budget ledger's contract, for both stores.
func contractLedger(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	day := Day.Start(now)
	k := BudgetKey{Kind: ScopeTeam, Name: "ledger-team"}
	a := Holder{Replica: "r1", Incarnation: "i1"}
	b := Holder{Replica: "r2", Incarnation: "i2"}
	ledger := func() LedgerState {
		t.Helper()
		st, err := s.Ledger(ctx, k, day)
		must(err)
		return st
	}

	// No budget: nothing to grant.
	res, err := s.GrowGrants(ctx, a, now, time.Minute, []GrantRequest{{Key: k, Start: day, Need: 1, Want: 1}})
	if err != nil || len(res) != 1 || !res[0].NoBudget {
		t.Fatalf("no budget: %+v %v", res, err)
	}
	// A request recorded before the budget exists counts in its period.
	must(s.InsertRecords(ctx, []Record{{ID: "ledger-before", Time: now, Team: k.Name, KeyID: "x", Model: "m", Cost: 0.1, CostNano: unitNano / 10}}))
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: Day, MaxCost: 1}, now, time.Minute, false))
	res, err = s.GrowGrants(ctx, a, now, time.Minute, []GrantRequest{{Key: k, Start: day, Need: 3 * unitNano / 10, Want: 5 * unitNano / 10}})
	if err != nil || res[0].Grown != 5*unitNano/10 || res[0].Room != 4*unitNano/10 || res[0].Epoch == 0 || !res[0].Until.IsZero() {
		t.Fatalf("grow: %+v %v", res, err)
	}
	if st := ledger(); st.Spent != unitNano/10 || st.Granted != 5*unitNano/10 {
		t.Fatalf("ledger %+v", st)
	}
	// What does not fit is refused, naming the room left.
	_, err = s.GrowGrants(ctx, b, now, time.Minute, []GrantRequest{{Key: k, Start: day, Need: 5 * unitNano / 10, Want: unitNano}})
	var nr *NoRoomError
	if !errors.As(err, &nr) || nr.Room != 4*unitNano/10 || nr.Key != k {
		t.Fatalf("no room: %v", err)
	}
	// All or nothing: a second budget that fits gets nothing either.
	k2 := BudgetKey{Kind: ScopeTag, Name: "ledger-tag"}
	must(s.SetBudget(ctx, Budget{BudgetKey: k2, Period: Day, MaxCost: 10}))
	_, err = s.GrowGrants(ctx, b, now, time.Minute, []GrantRequest{{Key: k2, Start: day, Need: 1, Want: 1}, {Key: k, Start: day, Need: unitNano, Want: unitNano}})
	if !errors.As(err, &nr) {
		t.Fatalf("all or nothing: %v", err)
	}
	if st, _ := s.Ledger(ctx, k2, day); st.Granted != 0 {
		t.Fatalf("partial growth kept: %+v", st)
	}
	// Settlement in the insert: spent += actual, grant -= min(actual, W).
	rec := Record{ID: "ledger-held", Time: now, AdmittedAt: now, Team: k.Name, KeyID: "x", Model: "m", Cost: 0.2, CostNano: 2 * unitNano / 10,
		HoldReplica: a.Replica, HoldIncarnation: a.Incarnation, HoldNano: 3 * unitNano / 10, Holds: []HoldRef{{Key: k, Start: day}}}
	must(s.InsertRecords(ctx, []Record{rec}))
	if st := ledger(); st.Spent != 3*unitNano/10 || st.Granted != 3*unitNano/10 {
		t.Fatalf("after settlement %+v", st)
	}
	must(s.InsertRecords(ctx, []Record{rec})) // a replay settles nothing
	if st := ledger(); st.Spent != 3*unitNano/10 || st.Granted != 3*unitNano/10 {
		t.Fatalf("after replay %+v", st)
	}
	// actual > W (a bug): the truth is charged, the grant loses only W.
	over := rec
	over.ID, over.CostNano, over.HoldNano = "ledger-over", 2*unitNano/10, unitNano/10
	must(s.InsertRecords(ctx, []Record{over}))
	if st := ledger(); st.Spent != 5*unitNano/10 || st.Granted != 2*unitNano/10 {
		t.Fatalf("after overshoot %+v", st)
	}

	// Lowering below spent + granted (0.7) is refused unless forced; a
	// forced lower takes a new epoch.
	before, _ := s.Budgets(ctx)
	epoch := findBudget(before, k).Epoch
	err = s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: Day, MaxCost: 0.6}, now, time.Minute, false)
	var bc *BelowCommittedError
	if !errors.As(err, &bc) || !errors.Is(err, ErrBelowCommitted) || bc.Spent != 5*unitNano/10 || bc.Granted != 2*unitNano/10 {
		t.Fatalf("lowering below committed: %v", err)
	}
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: Day, MaxCost: 0.8}, now, time.Minute, false)) // above committed
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: Day, MaxCost: 0.6}, now, time.Minute, true))
	after, _ := s.Budgets(ctx)
	if e := findBudget(after, k).Epoch; e <= epoch {
		t.Fatalf("epoch %d after %d", e, epoch)
	}
	// Over a forced lower limit: nothing grows, raising is never refused.
	if _, err := s.GrowGrants(ctx, a, now, time.Minute, []GrantRequest{{Key: k, Start: day, Need: 1, Want: 1}}); !errors.As(err, &nr) || nr.Room != 0 {
		t.Fatalf("growth over a forced limit: %v", err)
	}
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: Day, MaxCost: 0.65}, now, time.Minute, false))
	must(s.SetBudgetChecked(ctx, Budget{BudgetKey: k, Period: Day, MaxCost: 2}, now, time.Minute, false))

	// A temporary increase counts only when it outlives the lending time,
	// and its grants are lendable until its expiry minus that time.
	must(s.IncreaseBudgetChecked(ctx, k, 1, now.Add(30*time.Second), now, time.Minute, false))
	res, err = s.GrowGrants(ctx, b, now, time.Minute, []GrantRequest{{Key: k, Start: day, Need: 1, Want: 2 * unitNano}})
	if err != nil || res[0].Grown != 13*unitNano/10 || !res[0].Until.IsZero() {
		t.Fatalf("short increase lent: %+v %v", res, err)
	}
	must(s.IncreaseBudgetChecked(ctx, k, 1, now.Add(time.Hour), now, time.Minute, false))
	res, err = s.GrowGrants(ctx, b, now, time.Minute, []GrantRequest{{Key: k, Start: day, Need: 1, Want: 2 * unitNano}})
	if err != nil || res[0].Grown != unitNano || !res[0].Until.Equal(now.Add(59*time.Minute)) {
		t.Fatalf("increase lent: %+v %v", res, err)
	}

	// Returns never go below zero; a release drops a whole incarnation.
	must(s.ReturnGrants(ctx, a, []GrantReturn{{Key: k, Start: day, Amount: unitNano / 10}}))
	must(s.ReturnGrants(ctx, a, []GrantReturn{{Key: k, Start: day, Amount: 5 * unitNano}, {Key: k2, Start: day, Amount: 1}}))
	if st := ledger(); st.Granted != 23*unitNano/10 {
		t.Fatalf("after returns %+v", st)
	}
	gs, err := s.Grants(ctx)
	if err != nil || len(gs) != 1 || gs[0].Holder != b || gs[0].Granted != 23*unitNano/10 || gs[0].Key != k || !gs[0].Start.Equal(day) {
		t.Fatalf("grants %+v %v", gs, err)
	}
	n, err := s.ReleaseIncarnation(ctx, "i2")
	if err != nil || n != 23*unitNano/10 || ledger().Granted != 0 {
		t.Fatalf("release %d %v %+v", n, err, ledger())
	}
	_, err = s.GrowGrants(ctx, a, now, time.Minute, []GrantRequest{{Key: k, Start: day, Need: 1, Want: 1}})
	must(err)
	gs, _ = s.Grants(ctx)
	if n, err := s.ReleaseGrants(ctx, gs); err != nil || n != 1 || ledger().Granted != 0 {
		t.Fatalf("release rows %d %v", n, err)
	}
	if n, err := s.ReleaseGrants(ctx, gs); err != nil || n != 0 {
		t.Fatalf("release twice %d %v", n, err)
	}
	// A new period starts empty (its own usage only).
	tomorrow := day.AddDate(0, 0, 1)
	if _, err := s.GrowGrants(ctx, a, tomorrow, time.Minute, []GrantRequest{{Key: k, Start: tomorrow, Need: 2 * unitNano, Want: 2 * unitNano}}); err != nil {
		t.Fatalf("next period: %v", err)
	}
	if st, _ := s.Ledger(ctx, k, tomorrow); st.Spent != 0 || st.Granted != 2*unitNano {
		t.Fatalf("next period %+v", st)
	}
	if _, err := s.ReleaseIncarnation(ctx, "i1"); err != nil {
		t.Fatal(err)
	}
	// A deleted budget created again takes an epoch never seen before.
	must(s.DeleteBudget(ctx, k2))
	must(s.SetBudget(ctx, Budget{BudgetKey: k2, Period: Day, MaxCost: 10}))
	bs, _ := s.Budgets(ctx)
	if findBudget(bs, k2).Epoch == 0 {
		t.Fatal("re-created budget without an epoch")
	}
}

func findBudget(bs []Budget, k BudgetKey) Budget {
	for _, b := range bs {
		if b.BudgetKey == k {
			return b
		}
	}
	return Budget{}
}

func TestLedgerMemory(t *testing.T) { contractLedger(t, NewMemory()) }

func TestLimitNano(t *testing.T) {
	now := time.Unix(1000, 0)
	exp := now.Add(time.Hour)
	b := Budget{MaxCost: 1.5, TempIncrease: 1, TempExpiry: &exp}
	if l, until := LimitNano(b, now, time.Minute); l != 2_500_000_000 || !until.Equal(exp.Add(-time.Minute)) {
		t.Fatalf("%d %v", l, until)
	}
	if l, until := LimitNano(b, now, 2*time.Hour); l != 1_500_000_000 || !until.IsZero() {
		t.Fatalf("%d %v", l, until)
	}
	if ToNano(1e15) != MaxLedgerNano || ToNano(-1) != 0 || ToNano(0.1) != 100_000_000 {
		t.Fatal("ToNano")
	}
	if RecordCostNano(Record{Cost: 0.1}) != 100_000_000 || RecordCostNano(Record{CostNano: 5, Cost: 1}) != 5 {
		t.Fatal("RecordCostNano")
	}
	a := Attrs{Org: "o", Team: "t", Project: "p", KeyID: "k", User: "alice", Model: "m", Tags: []string{"x"}}
	ks := ScopeKeys(a)
	if len(ks) != 12 || !Matches(ks[6], a) || ks[8] != (BudgetKey{Kind: ScopeUser, Name: "alice"}) || !Matches(ks[9], a) {
		t.Fatalf("%+v", ks)
	}
	other := a
	other.Model = "other"
	if Matches(ks[1], other) || Matches(ks[8], Attrs{Org: "o", Team: "t", KeyID: "k2"}) {
		t.Fatal("matched another model, or a key without its user")
	}
	// One user, two keys (or a key and a token): one budget.
	ab := a
	ab.KeyID = "k2"
	if !Matches(BudgetKey{Kind: ScopeUser, Name: "alice"}, ab) {
		t.Fatal("the user budget does not follow the person")
	}
}
