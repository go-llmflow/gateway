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

// contractTools runs after contract (etabA exists).
func contractTools(t *testing.T, s Store) {
	ctx := context.Background()
	if err := s.SetTeamTools(ctx, "etabA", []string{"gh.*", "docs.search", "gh.*"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamTools(ctx, "nobody", []string{"*"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown team: %v", err)
	}
	tt, err := s.TeamTools(ctx)
	if err != nil || len(tt) != 1 || len(tt["etabA"]) != 2 || tt["etabA"][0] != "docs.search" || tt["etabA"][1] != "gh.*" {
		t.Fatalf("team tools %v %v", tt, err)
	}
	if err := s.SetTeamTools(ctx, "etabA", nil); err != nil {
		t.Fatal(err)
	}
	if tt, _ := s.TeamTools(ctx); len(tt) != 0 {
		t.Fatalf("cleared: %v", tt)
	}
}

func TestMemoryModels(t *testing.T) {
	m := NewMemory()
	contract(t, m)
	contractModels(t, m)
	contractTools(t, m)
	boom := errors.New("boom")
	m.SetFail(boom)
	_, err := m.TeamModels(context.Background())
	if !errors.Is(err, boom) || !errors.Is(m.SetTeamModels(context.Background(), "etabA", nil), boom) {
		t.Fatal("faults")
	}
}
