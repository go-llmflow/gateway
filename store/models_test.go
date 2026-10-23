package store

import (
	"context"
	"errors"
	"testing"
)

// contractModels runs after contract (etabA exists).
func contractModels(t *testing.T, s Store) {
	ctx := context.Background()
	if err := s.SetTeamModels(ctx, "etabA", []string{"b", "a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamModels(ctx, "nobody", []string{"a"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown team: %v", err)
	}
	tm, err := s.TeamModels(ctx)
	if err != nil || len(tm) != 1 || len(tm["etabA"]) != 2 || tm["etabA"][0] != "a" || tm["etabA"][1] != "b" {
		t.Fatalf("team models %v %v", tm, err)
	}
	if err := s.SetTeamModels(ctx, "etabA", nil); err != nil {
		t.Fatal(err)
	}
	if tm, _ := s.TeamModels(ctx); len(tm) != 0 {
		t.Fatalf("cleared: %v", tm)
	}
}

func TestMemoryModels(t *testing.T) {
	m := NewMemory()
	contract(t, m)
	contractModels(t, m)
	boom := errors.New("boom")
	m.SetFail(boom)
	_, err := m.TeamModels(context.Background())
	if !errors.Is(err, boom) || !errors.Is(m.SetTeamModels(context.Background(), "etabA", nil), boom) {
		t.Fatal("faults")
	}
}
