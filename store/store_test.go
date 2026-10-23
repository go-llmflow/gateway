package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// contract runs the same assertions against every implementation.
func contract(t *testing.T, s Store) {
	ctx := context.Background()
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateTeam(ctx, "etabA"))
	must(s.CreateTeam(ctx, "etabB"))
	if err := s.CreateTeam(ctx, "etabA"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate team: %v", err)
	}
	teams, err := s.ListTeams(ctx)
	must(err)
	if len(teams) != 2 || teams[0].Name != "etabA" || teams[0].Disabled {
		t.Fatalf("teams %+v", teams)
	}

	secret, id, h := GenerateKey()
	if !strings.HasPrefix(secret, KeyPrefix) || len(secret) != len(KeyPrefix)+43 || !HashEqual(h, HashSecret(secret)) {
		t.Fatalf("key %q", secret)
	}
	must(s.CreateKey(ctx, Key{ID: id, Hash: h, Team: "etabA", Name: "ci", RPM: 60, TPM: 1000, MaxInFlight: 4}))
	if err := s.CreateKey(ctx, Key{ID: id, Hash: HashSecret("x"), Team: "etabA"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate id: %v", err)
	}
	if err := s.CreateKey(ctx, Key{ID: "k_other", Hash: h, Team: "etabA"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate hash: %v", err)
	}
	if err := s.CreateKey(ctx, Key{ID: "k_x", Hash: HashSecret("y"), Team: "nobody"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key for unknown team: %v", err)
	}
	k, err := s.KeyByHash(ctx, HashSecret(secret))
	must(err)
	if k.ID != id || k.Team != "etabA" || k.RPM != 60 || k.TPM != 1000 || k.MaxInFlight != 4 || k.Revoked || k.Hash != h {
		t.Fatalf("key %+v", k)
	}
	if _, err := s.KeyByHash(ctx, HashSecret("wrong")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong key: %v", err)
	}
	_, id2, h2 := GenerateKey()
	must(s.CreateKey(ctx, Key{ID: id2, Hash: h2, Team: "etabB"}))
	keys, err := s.ListKeys(ctx, "etabA")
	must(err)
	if len(keys) != 1 {
		t.Fatalf("keys %+v", keys)
	}
	keys, err = s.ListKeys(ctx, "")
	must(err)
	if len(keys) != 2 {
		t.Fatalf("all keys %+v", keys)
	}
	must(s.RevokeKey(ctx, id))
	if k, _ := s.KeyByHash(ctx, h); !k.Revoked {
		t.Fatal("revoke")
	}
	if err := s.RevokeKey(ctx, "k_none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke unknown: %v", err)
	}

	must(s.SetBudget(ctx, Budget{Team: "etabB", Period: Month, MaxCost: 100}))
	must(s.SetBudget(ctx, Budget{Team: "etabB", Period: Day, MaxCost: 5}))
	if err := s.SetBudget(ctx, Budget{Team: "nobody", Period: Day, MaxCost: 5}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("budget for unknown team: %v", err)
	}
	if err := s.SetBudget(ctx, Budget{Team: "etabB", Period: "year", MaxCost: 5}); err == nil {
		t.Fatal("bad period accepted")
	}
	bs, err := s.Budgets(ctx)
	must(err)
	if len(bs) != 1 || bs[0].Period != Day || bs[0].MaxCost != 5 {
		t.Fatalf("budgets %+v", bs)
	}

	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	must(s.InsertRecords(ctx, []Record{
		{Time: t0, Team: "etabA", KeyID: id, Model: "m", Pool: "p", Upstream: "u", PromptTokens: 10, CompletionTokens: 5, CachedTokens: 2, Cost: 1.5, Status: 200},
		{Time: t0.Add(time.Hour), Team: "etabA", KeyID: id, Model: "m", Pool: "p", Upstream: "u", PromptTokens: 1, CompletionTokens: 1, Cost: 0.5, Status: 499, Borrowing: true, Priority: 10, Stream: true, Estimated: true},
		{Time: t0.Add(2 * time.Hour), Team: "etabB", KeyID: id2, Model: "m", Pool: "p", Upstream: "u", PromptTokens: 3, Cost: 0.25, Status: 200},
		{Time: t0.Add(48 * time.Hour), Team: "etabB", KeyID: id2, Model: "m", Pool: "q", Upstream: "u", Cost: 7, Status: 200},
	}))
	spend, err := s.SpendSince(ctx, t0.Add(time.Minute))
	must(err)
	if spend["etabA"] != 0.5 || spend["etabB"] != 7.25 {
		t.Fatalf("spend %v", spend)
	}
	us, err := s.Usage(ctx, t0, t0.Add(24*time.Hour))
	must(err)
	if len(us) != 2 || us[0].Team != "etabA" || us[0].Requests != 2 || us[0].PromptTokens != 11 ||
		us[0].CompletionTokens != 6 || us[0].CachedTokens != 2 || us[0].GPUHours != 2 || us[1].GPUHours != 0.25 ||
		us[0].EstimatedRequests != 1 || us[1].EstimatedRequests != 0 {
		t.Fatalf("usage %+v", us)
	}

	must(s.DisableTeam(ctx, "etabB"))
	if k, _ := s.KeyByHash(ctx, h2); !k.Revoked {
		t.Fatal("disabling a team must revoke its keys")
	}
	if err := s.CreateKey(ctx, Key{ID: "k_dis", Hash: HashSecret("z"), Team: "etabB"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key for disabled team: %v", err)
	}
	if err := s.DisableTeam(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disable unknown: %v", err)
	}
	must(s.CreateTeam(ctx, "etabB")) // re-enable
	must(s.DeleteBudget(ctx, "etabB"))
	if err := s.DeleteBudget(ctx, "etabB"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing budget: %v", err)
	}
}

func TestMemoryContract(t *testing.T) {
	m := NewMemory()
	contract(t, m)
	m.Close()
}

func TestMemoryFaults(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	m.MaxRecords = 2
	_ = m.InsertRecords(ctx, []Record{{Cost: 1}, {Cost: 2}, {Cost: 3}})
	if len(m.records) != 2 || m.records[0].Cost != 2 {
		t.Fatalf("bound: %+v", m.records)
	}
	boom := errors.New("boom")
	m.SetFail(boom)
	checks := []error{
		m.Ping(ctx), m.CreateTeam(ctx, "a"), m.DisableTeam(ctx, "a"), m.CreateKey(ctx, Key{}),
		m.RevokeKey(ctx, "a"), m.SetBudget(ctx, Budget{Period: Day}), m.DeleteBudget(ctx, "a"),
		m.InsertRecords(ctx, nil),
	}
	_, e1 := m.ListTeams(ctx)
	_, e2 := m.ListKeys(ctx, "")
	_, e3 := m.KeyByHash(ctx, [32]byte{})
	_, e4 := m.Budgets(ctx)
	_, e5 := m.SpendSince(ctx, time.Time{})
	_, e6 := m.Usage(ctx, time.Time{}, time.Time{})
	for i, err := range append(checks, e1, e2, e3, e4, e5, e6) {
		if !errors.Is(err, boom) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestPeriodStart(t *testing.T) {
	now := time.Date(2026, 10, 23, 18, 30, 0, 0, time.FixedZone("CEST", 2*3600)) // Friday
	if got := Day.Start(now); !got.Equal(time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day %v", got)
	}
	if got := Week.Start(now); !got.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("week %v", got)
	}
	sunday := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if got := Week.Start(sunday); !got.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("week from sunday %v", got)
	}
	if got := Month.Start(now); !got.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month %v", got)
	}
	if Period("year").Valid() {
		t.Fatal("year valid")
	}
	for _, b := range []Budget{{Period: Day, MaxCost: -1}, {Period: Day, MaxCost: 2e15}} {
		if ValidateBudget(b) == nil {
			t.Fatalf("accepted %+v", b)
		}
	}
}

// TestPostgresContract runs against a real PostgreSQL when
// LLMFLOW_TEST_PG_DSN names one (CI runs a postgres service container);
// it is skipped otherwise. It drops the gateway's tables first.
func TestPostgresContract(t *testing.T) {
	dsn := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	p, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx, `DROP TABLE IF EXISTS llmflow_usage, llmflow_budgets, llmflow_keys, llmflow_teams`); err != nil {
		t.Fatal(err)
	}
	p.Close()
	p, err = OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	contract(t, p)

	if _, err := OpenPostgres(ctx, "postgres://u:hunter2@[::1"); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("bad DSN error leaks or passes: %v", err)
	}
}
