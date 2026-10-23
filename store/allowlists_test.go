package store

import (
	"context"
	"errors"
	"testing"
)

func contractAllowlists(t *testing.T, s Store) {
	ctx := context.Background()
	if err := s.SetAllowlist(ctx, Allowlist{AllowKey: AllowKey{Kind: ScopeOrg, Name: "orgA"}, CIDRs: []string{"192.0.2.7/24", "2001:db8::1/32", "192.0.2.0/24"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAllowlist(ctx, Allowlist{AllowKey: AllowKey{Kind: ScopeKey, Name: "k_1"}, CIDRs: []string{"10.1.2.3/32"}}); err != nil {
		t.Fatal(err)
	}
	as, err := s.Allowlists(ctx)
	if err != nil || len(as) != 2 || as[0].Kind != ScopeKey || as[1].Name != "orgA" || len(as[1].CIDRs) != 2 || as[1].CIDRs[0] != "192.0.2.0/24" || as[1].CIDRs[1] != "2001:db8::/32" {
		t.Fatalf("allowlists %+v %v", as, err)
	}
	if err := s.SetAllowlist(ctx, Allowlist{AllowKey: AllowKey{Kind: ScopeKey, Name: "k_1"}}); err != nil {
		t.Fatal(err)
	}
	if as, _ := s.Allowlists(ctx); len(as) != 1 {
		t.Fatalf("removal %+v", as)
	}
	for _, bad := range []Allowlist{
		{AllowKey: AllowKey{Kind: ScopeTag, Name: "x"}},
		{AllowKey: AllowKey{Kind: ScopeOrg, Name: ""}},
		{AllowKey: AllowKey{Kind: ScopeOrg, Name: "o"}, CIDRs: []string{"10.0.0.1"}},
		{AllowKey: AllowKey{Kind: ScopeOrg, Name: "o"}, CIDRs: []string{"300.0.0.0/8"}},
	} {
		if err := s.SetAllowlist(ctx, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestMemoryAllowlists(t *testing.T) {
	m := NewMemory()
	contractAllowlists(t, m)
	boom := errors.New("boom")
	m.SetFail(boom)
	if _, err := m.Allowlists(context.Background()); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := m.SetAllowlist(context.Background(), Allowlist{AllowKey: AllowKey{Kind: ScopeOrg, Name: "o"}}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
