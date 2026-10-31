package gateway

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// pick encodes a record of adoptionFixture: a person (0-7), a model (0-3) and
// a day (0-7 from a Monday, so two ISO weeks).
func pick(user, model, day int) byte { return byte(user&7 | (model&3)<<3 | (day&7)<<5) }

// adoptionFixture stores one record per byte, decoded as pick encodes it.
func adoptionFixture(t *testing.T, picks []byte) (*store.Memory, []store.Record) {
	t.Helper()
	st := store.NewMemory()
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) // a Monday
	var recs []store.Record
	for _, p := range picks {
		recs = append(recs, store.Record{
			Time: at.AddDate(0, 0, int(p>>5)), Org: "etabA", Team: "etabA",
			User: "u" + strconv.Itoa(int(p&7)), KeyID: "k", Model: "m" + strconv.Itoa(int(p>>3)&3), Pool: "b300", Status: 200,
		})
	}
	if err := st.InsertRecords(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	return st, recs
}

// planned reads and plans the adoption of the fixture's two weeks, as
// adoptionOf does.
func planned(t *testing.T, st *store.Memory, k int) adoptionView {
	t.Helper()
	ctx := context.Background()
	from, to := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	a, err := st.Adoption(ctx, from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := planAdoption(a, k, func(dim string, keys []string) (int64, error) {
		return st.UsersOf(ctx, from, to, nil, dim, keys)
	}); err != nil {
		t.Fatal(err)
	}
	return suppressAdoption(a, k)
}

func checkAdoptionResidual(t *testing.T, v adoptionView, recs []store.Record, k int) {
	t.Helper()
	if err := adoptionResidual(v, recs, k); err != nil {
		t.Fatal(err)
	}
}

// adoptionResidual says when a sum the view publishes, minus its shown parts,
// is the requests of 1 to k-1 distinct people. The people are counted from the
// records, not by the store, so the check does not share the code it checks.
func adoptionResidual(v adoptionView, recs []store.Record, k int) error {
	people := func(in func(store.Record) bool) int {
		u := map[string]bool{}
		for _, r := range recs {
			if in(r) {
				u[r.User] = true
			}
		}
		return len(u)
	}
	day := func(r store.Record) string { return r.Time.UTC().Format("2006-01-02") }
	hiddenSet := func(ps []adoptionPoint) map[string]bool {
		h := map[string]bool{}
		for _, p := range ps {
			if p.Suppressed {
				h[p.Period] = true
			}
		}
		return h
	}
	hDays, hWeeks := hiddenSet(v.Daily), hiddenSet(v.Weekly)
	hModels := map[string]bool{}
	shown := map[string]bool{}
	for _, m := range v.Models {
		if m.Model != OtherModels {
			shown[m.Model] = true
		}
	}
	for _, r := range recs {
		if !shown[r.Model] {
			hModels[r.Model] = true
		}
	}
	small := func(n int) bool { return n > 0 && n < k }
	// The period's requests are known when one table publishes all of them:
	// every day, every week, or every model (with the merged row).
	totalKnown := len(hDays) == 0 || len(hWeeks) == 0
	modelsComplete := true
	for _, m := range v.Models {
		if m.Suppressed {
			modelsComplete = false
		}
	}
	totalKnown = totalKnown || modelsComplete
	if totalKnown {
		if n := people(func(r store.Record) bool { return hDays[day(r)] }); small(n) {
			return fmt.Errorf("total minus the shown days leaves %d people (< %d): %+v", n, k, v.Daily)
		}
		if n := people(func(r store.Record) bool { return hWeeks[mondayOf(day(r))] }); small(n) {
			return fmt.Errorf("total minus the shown weeks leaves %d people (< %d): %+v", n, k, v.Weekly)
		}
		if n := people(func(r store.Record) bool { return hModels[r.Model] }); small(n) {
			return fmt.Errorf("total minus the shown models leaves %d people (< %d): %+v", n, k, v.Models)
		}
	}
	for _, w := range v.Weekly {
		if w.Suppressed {
			continue
		}
		if n := people(func(r store.Record) bool { return mondayOf(day(r)) == w.Period && hDays[day(r)] }); small(n) {
			return fmt.Errorf("week %s minus its shown days leaves %d people (< %d)", w.Period, n, k)
		}
	}
	for _, m := range v.Models {
		if m.Model == OtherModels && !m.Suppressed && small(int(*m.ActiveUsers)) {
			return fmt.Errorf("merged row of %d people", *m.ActiveUsers)
		}
	}
	return nil
}

// The control: without the plan (primary suppression only), the check must
// see the leaks this change closes, or a green fuzz run proves nothing.
func TestAdoptionResidualCheckSeesTheLeak(t *testing.T) {
	cases := map[string][]byte{
		"week minus a shown day":    {0, 1, 2, 3, 4, pick(5, 0, 1)}, // day 1: u5 alone; the week holds 6
		"total minus a shown model": {0, 1, 2, 3, 4, pick(5, 1, 0)}, // m1: u5 alone
	}
	for name, picks := range cases {
		st, recs := adoptionFixture(t, picks)
		from, to := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
		a, err := st.Adoption(context.Background(), from, to, nil)
		if err != nil {
			t.Fatal(err)
		}
		if adoptionResidual(suppressAdoption(a, 5), recs, 5) == nil {
			t.Errorf("%s: the check passes an unprotected view", name)
		}
		if err := adoptionResidual(planned(t, st, 5), recs, 5); err != nil {
			t.Errorf("%s: the plan leaves %v", name, err)
		}
	}
}

// A single hidden model, never merged before this change, was the period's
// total minus the shown models.
func TestAdoptionSingleHiddenModelNotRecoverable(t *testing.T) {
	var picks []byte
	for u := byte(0); u < 5; u++ { // five people on m0, day 0
		picks = append(picks, u)
	}
	picks = append(picks, pick(5, 1, 0)) // u5 alone on m1
	st, recs := adoptionFixture(t, picks)
	v := planned(t, st, 5)
	for _, m := range v.Models {
		if m.Model == "m0" {
			t.Fatalf("m0 shown next to a one-person model: %+v", v.Models)
		}
	}
	checkAdoptionResidual(t, v, recs, 5)
}

// Without a union count, the largest hidden cell bounds the union from below:
// the plan hides at least as much.
func TestAdoptionLowerBoundUnionIsConservative(t *testing.T) {
	st, _ := adoptionFixture(t, []byte{0, 1, 2, 3, 4, pick(5, 1, 0), pick(6, 2, 1)})
	from, to := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	exact, err := st.Adoption(context.Background(), from, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	bound, _ := st.Adoption(context.Background(), from, to, nil)
	if err := planAdoption(exact, 5, func(dim string, keys []string) (int64, error) {
		return st.UsersOf(context.Background(), from, to, nil, dim, keys)
	}); err != nil {
		t.Fatal(err)
	}
	if err := planAdoption(bound, 5, lowerBoundUnion(bound)); err != nil {
		t.Fatal(err)
	}
	for dim, keys := range exact.Hide {
		for key := range keys {
			if !bound.Hide[dim][key] && !hidden(cellUsers(bound, dim, key), 5) {
				t.Fatalf("%s %s hidden with the exact union but shown with the bound", dim, key)
			}
		}
	}
}

func cellUsers(a *store.Adoption, dim, key string) int64 {
	switch dim {
	case store.DimDay:
		for _, p := range a.Daily {
			if p.Day == key {
				return p.ActiveUsers
			}
		}
	case store.DimWeek:
		for _, p := range a.Weekly {
			if p.Day == key {
				return p.ActiveUsers
			}
		}
	case store.DimModel:
		for _, m := range a.Models {
			if m.Model == key {
				return m.Users
			}
		}
	}
	return 0
}

// Fuzz: whatever the spread of people over days, weeks and models, no
// published sum minus its shown parts is the requests of 1 to k-1 people.
func FuzzAdoptionSubtraction(f *testing.F) {
	f.Add(uint8(3), []byte{0, 1, 2, 3, 4, pick(5, 1, 0)})
	f.Add(uint8(3), []byte{0, 1, 2, 3, 4, pick(5, 0, 1), pick(6, 2, 6)})
	f.Add(uint8(2), []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77})
	f.Fuzz(func(t *testing.T, k uint8, picks []byte) {
		if len(picks) > 200 {
			picks = picks[:200]
		}
		kk := int(k%6) + 2
		st, recs := adoptionFixture(t, picks)
		checkAdoptionResidual(t, planned(t, st, kk), recs, kk)
	})
}
