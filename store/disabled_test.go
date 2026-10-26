package store

import (
	"context"
	"os"
	"testing"
)

// Security audit 2026-10-26, finding A4: disabling a team revokes its keys
// in the same transaction, but a key created concurrently (its INSERT saw
// the team enabled, the revoking UPDATE did not see the INSERT) stays valid
// in a disabled team, and KeyByHash returned it as valid: the team's
// disabled flag was never read on the request path. A key of a disabled
// team must read as revoked, whatever state the race left behind.
func checkDisabledTeamKey(t *testing.T, st Store, disableTeamOnly func(team string)) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateOrg(ctx, "o4"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTeam(ctx, "t4", "o4"); err != nil {
		t.Fatal(err)
	}
	_, id, h := GenerateKey()
	if err := st.CreateKey(ctx, Key{ID: id, Hash: h, Team: "t4"}); err != nil {
		t.Fatal(err)
	}
	if k, err := st.KeyByHash(ctx, h); err != nil || k.Revoked {
		t.Fatalf("before: %+v %v", k, err)
	}
	// The state the race leaves: the team disabled, its key not revoked.
	disableTeamOnly("t4")
	k, err := st.KeyByHash(ctx, h)
	if err == nil && !k.Revoked {
		t.Fatalf("a key of a disabled team reads as valid: %+v", k)
	}
}

func TestDisabledProjectKeyMemory(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if err := m.CreateOrg(ctx, "o5"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateTeam(ctx, "t5", "o5"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateProject(ctx, "t5", "p5"); err != nil {
		t.Fatal(err)
	}
	_, id, h := GenerateKey()
	if err := m.CreateKey(ctx, Key{ID: id, Hash: h, Team: "t5", Project: "p5"}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.projects[[2]string{"t5", "p5"}].Disabled = true
	m.mu.Unlock()
	if k, err := m.KeyByHash(ctx, h); err == nil && !k.Revoked {
		t.Fatalf("a key of a disabled project reads as valid: %+v", k)
	}
}

func TestDisabledTeamKeyMemory(t *testing.T) {
	m := NewMemory()
	checkDisabledTeamKey(t, m, func(team string) {
		m.mu.Lock()
		m.teams[team].Disabled = true
		m.mu.Unlock()
	})
}

func TestDisabledTeamKeyPostgres(t *testing.T) {
	dsn := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	p, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dropAll(t, p)
	p.Close()
	if p, err = OpenPostgres(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	checkDisabledTeamKey(t, p, func(team string) {
		if _, err := p.pool.Exec(ctx, `UPDATE llmflow_teams SET disabled = true WHERE name = $1`, team); err != nil {
			t.Fatal(err)
		}
	})
}
