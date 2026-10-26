package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// contractUnits: costs of different units are never summed. SpendRows and
// Report split by unit, and a scope can hold one budget per unit.
func contractUnits(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2031, 3, 4, 10, 0, 0, 0, time.UTC)
	recs := []Record{
		{ID: "u-gpu-1", Time: at, Org: "unitorg", Team: "unitteam", KeyID: "k", Model: "m", Pool: "p", Cost: 2, PromptTokens: 10},
		{ID: "u-usd-1", Time: at, Org: "unitorg", Team: "unitteam", KeyID: "k", Model: "m", Pool: "p", Cost: 3, Unit: "USD", PromptTokens: 20},
		{ID: "u-usd-2", Time: at.Add(time.Minute), Org: "unitorg", Team: "unitteam", KeyID: "k", Model: "m", Pool: "p", Cost: 0.5, Unit: "USD", PromptTokens: 5},
	}
	if err := s.InsertRecords(ctx, recs); err != nil {
		t.Fatal(err)
	}
	rows, err := s.SpendRows(ctx, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, r := range rows {
		if r.Org == "unitorg" {
			got[r.Unit] += r.Cost
		}
	}
	if len(got) != 2 || got[""] != 2 || got["USD"] != 3.5 {
		t.Fatalf("spend by unit %v, want gpu 2 and USD 3.5 apart", got)
	}
	rep, err := s.Report(ctx, at.Add(-time.Hour), at.Add(time.Hour), []string{"org"})
	if err != nil {
		t.Fatal(err)
	}
	var mine []ReportRow
	for _, r := range rep {
		if r.Org == "unitorg" {
			mine = append(mine, r)
		}
	}
	if len(mine) != 2 {
		t.Fatalf("report rows %+v, want one per unit", mine)
	}
	for _, r := range mine {
		switch r.Unit {
		case "":
			if r.Cost != 2 || r.GPUHours != 2 || r.Requests != 1 || r.PromptTokens != 10 {
				t.Errorf("gpu row %+v", r)
			}
		case "USD":
			if r.Cost != 3.5 || r.GPUHours != 0 || r.Requests != 2 || r.PromptTokens != 25 {
				t.Errorf("USD row %+v", r)
			}
		default:
			t.Errorf("unexpected unit %q", r.Unit)
		}
		b, _ := json.Marshal(r)
		if want := `"unit":"` + UnitName(r.Unit) + `"`; !strings.Contains(string(b), want) {
			t.Errorf("JSON %s lacks %s", b, want)
		}
	}

	gpu := Budget{BudgetKey: BudgetKey{Kind: ScopeOrg, Name: "unitorg"}, Period: Month, MaxCost: 100}
	usd := Budget{BudgetKey: BudgetKey{Kind: ScopeOrg, Name: "unitorg", Unit: "USD"}, Period: Month, MaxCost: 50}
	for _, b := range []Budget{gpu, usd} {
		if err := s.SetBudget(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	find := func() map[string]Budget {
		bs, err := s.Budgets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Budget{}
		for _, b := range bs {
			if b.Name == "unitorg" {
				out[b.Unit] = b
			}
		}
		return out
	}
	if bs := find(); len(bs) != 2 || bs[""].MaxCost != 100 || bs["USD"].MaxCost != 50 {
		t.Fatalf("budgets per unit %+v", bs)
	}
	if err := s.IncreaseBudget(ctx, usd.BudgetKey, 5, at.AddDate(10, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if bs := find(); bs["USD"].TempIncrease != 5 || bs[""].TempIncrease != 0 {
		t.Fatalf("an increase of the USD budget reached another unit: %+v", bs)
	}
	if ok, err := s.MarkSoftAlert(ctx, usd.BudgetKey, at); err != nil || !ok {
		t.Fatalf("soft alert: %v %v", ok, err)
	}
	if ok, err := s.MarkSoftAlert(ctx, gpu.BudgetKey, at); err != nil || !ok {
		t.Fatalf("the GPU-hours budget's alert is its own: %v %v", ok, err)
	}
	if err := s.DeleteBudget(ctx, usd.BudgetKey); err != nil {
		t.Fatal(err)
	}
	if bs := find(); len(bs) != 1 || bs[""].MaxCost != 100 {
		t.Fatalf("deleting the USD budget touched the GPU-hours one: %+v", bs)
	}
	if err := s.DeleteBudget(ctx, gpu.BudgetKey); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBudget(ctx, usd.BudgetKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestUnitsMemory(t *testing.T) { contractUnits(t, NewMemory()) }

func TestNormalizeUnit(t *testing.T) {
	for in, want := range map[string]string{"": "", "gpu_hours": "", "USD": "USD", "eur_cents": "eur_cents"} {
		if got, err := NormalizeUnit(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"US D", "€", "a-b", strings.Repeat("X", 17)} {
		if _, err := NormalizeUnit(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	b := Budget{BudgetKey: BudgetKey{Kind: ScopeOrg, Name: "o", Unit: "gpu_hours"}, Period: Day}
	if err := ValidateBudget(b); err == nil {
		t.Error("a budget with the spelled-out GPU-hours unit must be normalised first")
	}
	b.Unit = "bad unit"
	if err := ValidateBudget(b); err == nil {
		t.Error("bad unit accepted")
	}
	if k := (BudgetKey{Kind: ScopeOrg, Name: "o", Unit: "USD"}); k.String() == (BudgetKey{Kind: ScopeOrg, Name: "o"}).String() {
		t.Error("two units, one key string")
	}
}
