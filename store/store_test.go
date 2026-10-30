package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
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
	must(s.CreateOrg(ctx, "orgA"))
	must(s.CreateOrg(ctx, "orgB"))
	if err := s.CreateOrg(ctx, "orgA"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate org: %v", err)
	}
	orgs, err := s.ListOrgs(ctx)
	must(err)
	if len(orgs) != 2 || orgs[0].Name != "orgA" || orgs[0].Disabled {
		t.Fatalf("orgs %+v", orgs)
	}
	if err := s.CreateTeam(ctx, "etabA", "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("team in unknown org: %v", err)
	}
	must(s.CreateTeam(ctx, "etabA", "orgA"))
	must(s.CreateTeam(ctx, "etabB", "orgB"))
	if err := s.CreateTeam(ctx, "etabA", "orgA"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate team: %v", err)
	}
	if err := s.CreateTeam(ctx, "etabA", "orgB"); !errors.Is(err, ErrConflict) {
		t.Fatalf("team moved to another org: %v", err)
	}
	teams, err := s.ListTeams(ctx, "")
	must(err)
	if len(teams) != 2 || teams[0].Name != "etabA" || teams[0].Org != "orgA" || teams[0].Disabled {
		t.Fatalf("teams %+v", teams)
	}
	teams, err = s.ListTeams(ctx, "orgB")
	must(err)
	if len(teams) != 1 || teams[0].Name != "etabB" {
		t.Fatalf("teams of orgB %+v", teams)
	}
	if tm, err := s.GetTeam(ctx, "etabB"); err != nil || tm.Org != "orgB" {
		t.Fatalf("GetTeam %+v %v", tm, err)
	}
	if _, err := s.GetTeam(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTeam unknown: %v", err)
	}

	// Projects.
	must(s.CreateProject(ctx, "etabA", "llm-eval"))
	if err := s.CreateProject(ctx, "etabA", "llm-eval"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate project: %v", err)
	}
	if err := s.CreateProject(ctx, "nobody", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("project of unknown team: %v", err)
	}
	ps, err := s.ListProjects(ctx, "etabA")
	must(err)
	if len(ps) != 1 || ps[0].Name != "llm-eval" || ps[0].Team != "etabA" {
		t.Fatalf("projects %+v", ps)
	}
	if err := s.CreateKey(ctx, Key{ID: "k_np", Hash: HashSecret("np"), Team: "etabA", Project: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key in unknown project: %v", err)
	}
	_, pid, ph := GenerateKey()
	must(s.CreateKey(ctx, Key{ID: pid, Hash: ph, Team: "etabA", Project: "llm-eval"}))
	if pk, err := s.GetKey(ctx, pid); err != nil || pk.Project != "llm-eval" || pk.Org != "orgA" {
		t.Fatalf("project key %+v %v", pk, err)
	}
	must(s.DisableProject(ctx, "etabA", "llm-eval"))
	if pk, _ := s.KeyByHash(ctx, ph); !pk.Revoked {
		t.Fatal("disabling a project must revoke its keys")
	}
	if err := s.DisableProject(ctx, "etabA", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disable unknown project: %v", err)
	}
	must(s.CreateProject(ctx, "etabA", "llm-eval")) // re-enable
	if _, err := s.GetKey(ctx, "k_none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetKey unknown: %v", err)
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
	if k.ID != id || k.Team != "etabA" || k.Org != "orgA" || k.Project != "" || k.RPM != 60 || k.TPM != 1000 || k.MaxInFlight != 4 || k.Revoked || k.Hash != h {
		t.Fatalf("key %+v", k)
	}
	if _, err := s.KeyByHash(ctx, HashSecret("wrong")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong key: %v", err)
	}
	_, id2, h2 := GenerateKey()
	must(s.CreateKey(ctx, Key{ID: id2, Hash: h2, Team: "etabB"}))
	keys, err := s.ListKeys(ctx, "etabB")
	must(err)
	if len(keys) != 1 || keys[0].Org != "orgB" {
		t.Fatalf("keys %+v", keys)
	}
	keys, err = s.ListKeys(ctx, "")
	must(err)
	if len(keys) != 3 {
		t.Fatalf("all keys %+v", keys)
	}
	must(s.RevokeKey(ctx, id))
	if k, _ := s.KeyByHash(ctx, h); !k.Revoked {
		t.Fatal("revoke")
	}
	if err := s.RevokeKey(ctx, "k_none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke unknown: %v", err)
	}

	// Rotation with a grace period: both secrets find the key, which keeps
	// its ID; the old one only as the previous secret.
	at := time.Now().UTC().Truncate(time.Microsecond)
	_, h3 := NewKeySecret()
	var none [32]byte
	must(s.RotateKey(ctx, id2, h3, none, at.Add(time.Hour), at))
	for _, hh := range [][32]byte{h2, h3} {
		k, err := s.KeyByHash(ctx, hh)
		must(err)
		if k.ID != id2 || k.Hash != h3 || k.PrevHash != h2 || !k.PrevExpires.Equal(at.Add(time.Hour)) || !k.RotatedAt.Equal(at) {
			t.Fatalf("rotated key %+v", k)
		}
		if !k.Matches(hh, at) || k.Matches(h2, at.Add(2*time.Hour)) || !k.Matches(h3, at.Add(2*time.Hour)) {
			t.Fatal("Matches")
		}
	}
	// Without grace the old secrets are gone at once.
	_, h4 := NewKeySecret()
	must(s.RotateKey(ctx, id2, h4, none, time.Time{}, at))
	for _, hh := range [][32]byte{h2, h3} {
		if _, err := s.KeyByHash(ctx, hh); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old secret after rotation without grace: %v", err)
		}
	}
	if k, err := s.KeyByHash(ctx, h4); err != nil || !k.PrevExpires.IsZero() {
		t.Fatalf("rotated without grace: %+v %v", k, err)
	}
	if err := s.RotateKey(ctx, id2, h4, none, time.Time{}, at); !errors.Is(err, ErrExists) {
		t.Fatalf("rotate to the current hash: %v", err)
	}
	if err := s.RotateKey(ctx, id, HashSecret("r"), none, time.Time{}, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate a revoked key: %v", err)
	}
	if err := s.RotateKey(ctx, "k_none", HashSecret("r"), none, time.Time{}, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate unknown: %v", err)
	}
	// Conditional rotation: only from the expected current hash.
	_, h5 := NewKeySecret()
	if err := s.RotateKey(ctx, id2, h5, h3, at.Add(time.Hour), at); !errors.Is(err, ErrConflict) {
		t.Fatalf("rotate from a stale hash: %v", err)
	}
	must(s.RotateKey(ctx, id2, h5, h4, at.Add(time.Hour), at))
	// Undoing it: back to the previous hash, conditional on the new one,
	// with no grace and no rotation time.
	must(s.RotateKey(ctx, id2, h4, h5, time.Time{}, time.Time{}))
	if k, err := s.KeyByHash(ctx, h4); err != nil || k.Hash != h4 || !k.PrevExpires.IsZero() || !k.RotatedAt.IsZero() {
		t.Fatalf("undone rotation: %+v %v", k, err)
	}
	if _, err := s.KeyByHash(ctx, h5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("undone secret still found: %v", err)
	}
	// Schedules: due when created (or rotated) + every <= now.
	sched := Rotation{Every: Duration(time.Hour), Grace: Duration(time.Minute), Target: "vault:secret/keys/etabB/k#key"}
	must(s.SetKeyRotation(ctx, id2, sched))
	if err := s.SetKeyRotation(ctx, id, sched); !errors.Is(err, ErrNotFound) {
		t.Fatalf("schedule a revoked key: %v", err)
	}
	k2, err := s.GetKey(ctx, id2)
	must(err)
	if k2.Rotation != sched {
		t.Fatalf("schedule %+v", k2.Rotation)
	}
	due, err := s.DueKeys(ctx, k2.CreatedAt.Add(59*time.Minute))
	must(err)
	if len(due) != 0 {
		t.Fatalf("due too early: %+v", due)
	}
	due, err = s.DueKeys(ctx, k2.CreatedAt.Add(61*time.Minute))
	must(err)
	if len(due) != 1 || due[0].ID != id2 || due[0].Rotation != sched {
		t.Fatalf("due: %+v", due)
	}
	must(s.RotateKey(ctx, id2, h5, h4, time.Time{}, k2.CreatedAt.Add(61*time.Minute)))
	if due, _ = s.DueKeys(ctx, k2.CreatedAt.Add(90*time.Minute)); len(due) != 0 {
		t.Fatalf("due again right after a rotation: %+v", due)
	}
	must(s.SetKeyRotation(ctx, id2, Rotation{}))
	h2 = h5 // later checks use the current secret

	// A key's request policy is kept.
	_, pid2, ph2 := GenerateKey()
	must(s.CreateKey(ctx, Key{ID: pid2, Hash: ph2, Team: "etabA", Policy: "strict", Guardrails: []string{"pii", "secrets", "pii"}}))
	if k, err := s.GetKey(ctx, pid2); err != nil || k.Policy != "strict" || len(k.Guardrails) != 2 || k.Guardrails[0] != "pii" {
		t.Fatalf("key policy and guardrails: %+v %v", k, err)
	}
	must(s.RevokeKey(ctx, pid2))

	// SCIM documents.
	ua := SCIMDoc{Kind: "User", ID: "u1", Name: "Alice@Example.org", Data: []byte(`{"userName":"Alice@Example.org"}`), Version: "v1"}
	must(s.SCIMCreate(ctx, ua))
	if err := s.SCIMCreate(ctx, SCIMDoc{Kind: "User", ID: "u2", Name: "alice@example.ORG", Data: []byte(`{}`), Version: "v1"}); !errors.Is(err, ErrExists) {
		t.Fatalf("userName taken without case: %v", err)
	}
	if err := s.SCIMCreate(ctx, ua); !errors.Is(err, ErrExists) {
		t.Fatalf("id taken: %v", err)
	}
	must(s.SCIMCreate(ctx, SCIMDoc{Kind: "User", ID: "u2", Name: "bob", Data: []byte(`{"userName":"bob"}`), Version: "v1"}))
	for _, gid := range []string{"g1", "g2"} { // group names need not be unique
		must(s.SCIMCreate(ctx, SCIMDoc{Kind: "Group", ID: gid, Name: "Lab", Data: []byte(`{"displayName":"Lab"}`), Version: "v1"}))
	}
	d, err := s.SCIMGet(ctx, "User", "u1")
	must(err)
	var data map[string]any
	if json.Unmarshal(d.Data, &data) != nil || data["userName"] != "Alice@Example.org" || d.Name != ua.Name || d.Version != "v1" {
		t.Fatalf("scim doc %+v", d)
	}
	if _, err := s.SCIMGet(ctx, "Group", "u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kind is part of the key: %v", err)
	}
	ua.Data, ua.Version = []byte(`{"userName":"alice2"}`), "v2"
	ua.Name = "alice2"
	if err := s.SCIMReplace(ctx, ua, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale replace: %v", err)
	}
	must(s.SCIMReplace(ctx, ua, "v1"))
	ub := SCIMDoc{Kind: "User", ID: "u2", Name: "ALICE2", Data: []byte(`{}`), Version: "v2"}
	if err := s.SCIMReplace(ctx, ub, "v1"); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto a taken userName: %v", err)
	}
	if err := s.SCIMReplace(ctx, SCIMDoc{Kind: "User", ID: "nope", Name: "x", Data: []byte(`{}`), Version: "v"}, "v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replace unknown: %v", err)
	}
	ds, err := s.SCIMList(ctx, "Group", "LAB")
	must(err)
	if len(ds) != 2 {
		t.Fatalf("groups by name: %+v", ds)
	}
	if ds, _ = s.SCIMList(ctx, "User", ""); len(ds) != 2 {
		t.Fatalf("all users: %+v", ds)
	}
	must(s.SCIMDelete(ctx, "User", "u2"))
	if err := s.SCIMDelete(ctx, "User", "u2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}

	teamB := BudgetKey{Kind: ScopeTeam, Name: "etabB"}
	must(s.SetBudget(ctx, Budget{BudgetKey: teamB, Period: Month, MaxCost: 100}))
	must(s.SetBudget(ctx, Budget{BudgetKey: teamB, Period: Day, MaxCost: 5, SoftLimit: 4}))
	must(s.SetBudget(ctx, Budget{BudgetKey: BudgetKey{Kind: ScopeKey, Name: id, Model: "m"}, Period: Week, MaxCost: 2}))
	for _, bad := range []Budget{
		{BudgetKey: teamB, Period: "year", MaxCost: 5},
		{BudgetKey: BudgetKey{Kind: "person", Name: "x"}, Period: Day},
		{BudgetKey: BudgetKey{Kind: ScopeTag, Name: ""}, Period: Day},
		{BudgetKey: teamB, Period: Day, MaxCost: -1},
		{BudgetKey: teamB, Period: Day, SoftLimit: -1},
	} {
		if err := s.SetBudget(ctx, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	must(s.IncreaseBudget(ctx, teamB, 3, exp))
	if err := s.IncreaseBudget(ctx, BudgetKey{Kind: ScopeTag, Name: "nope"}, 1, exp); !errors.Is(err, ErrNotFound) {
		t.Fatalf("increase unknown: %v", err)
	}
	must(s.SetBudget(ctx, Budget{BudgetKey: teamB, Period: Day, MaxCost: 5, SoftLimit: 4})) // keeps the increase
	bs, err := s.Budgets(ctx)
	must(err)
	if len(bs) != 2 || bs[1].Kind != ScopeTeam || bs[1].Period != Day || bs[1].MaxCost != 5 || bs[1].SoftLimit != 4 ||
		bs[1].TempIncrease != 3 || bs[1].TempExpiry == nil || !bs[1].TempExpiry.Equal(exp) {
		t.Fatalf("budgets %+v", bs)
	}
	if l := bs[1].Limit(time.Now()); l != 8 {
		t.Fatalf("limit with increase %v", l)
	}
	if l := bs[1].Limit(exp.Add(time.Second)); l != 5 {
		t.Fatalf("limit after expiry %v", l)
	}
	p0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if ok, err := s.MarkSoftAlert(ctx, teamB, p0); !ok || err != nil {
		t.Fatalf("first alert %v %v", ok, err)
	}
	if ok, err := s.MarkSoftAlert(ctx, teamB, p0); ok || err != nil {
		t.Fatalf("second alert in the same period %v %v", ok, err)
	}
	if ok, err := s.MarkSoftAlert(ctx, teamB, p0.AddDate(0, 0, 1)); !ok || err != nil {
		t.Fatalf("alert in the next period %v %v", ok, err)
	}
	if _, err := s.MarkSoftAlert(ctx, BudgetKey{Kind: ScopeTag, Name: "nope"}, p0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("alert unknown: %v", err)
	}

	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	must(s.InsertRecords(ctx, []Record{
		{Time: t0, Org: "orgA", Team: "etabA", KeyID: id, Model: "m", Pool: "p", Upstream: "u", PromptTokens: 10, CompletionTokens: 5, CachedTokens: 2, Cost: 1.5, Status: 200, Tags: []string{"eval"}},
		{Time: t0.Add(time.Hour), Org: "orgA", Team: "etabA", KeyID: id, Model: "m", Pool: "p", Upstream: "u", PromptTokens: 1, CompletionTokens: 1, Cost: 0.5, Status: 499, Borrowing: true, Priority: 10, Stream: true, Estimated: true, Tags: []string{"eval", "prod"}},
		{Time: t0.Add(2 * time.Hour), Org: "orgB", Team: "etabB", KeyID: id2, Model: "m", Pool: "p", Upstream: "u", PromptTokens: 3, Cost: 0.25, Status: 200},
		{Time: t0.Add(48 * time.Hour), Org: "orgB", Team: "etabB", Project: "x", KeyID: id2, Model: "m", Pool: "q", Upstream: "u", Cost: 7, Status: 200},
	}))
	rows, err := s.SpendRows(ctx, t0.Add(time.Minute))
	must(err)
	sum := map[string]float64{}
	for _, r := range rows {
		sum[r.Team] += r.Cost
		for _, tg := range r.Tags {
			sum["tag:"+tg] += r.Cost
		}
		if r.Project != "" {
			sum["project:"+r.Project] += r.Cost
		}
	}
	if len(rows) != 3 || sum["etabA"] != 0.5 || sum["etabB"] != 7.25 || sum["tag:prod"] != 0.5 || sum["tag:eval"] != 0.5 || sum["project:x"] != 7 {
		t.Fatalf("spend rows %+v", rows)
	}
	us, err := s.Report(ctx, t0, t0.Add(24*time.Hour), []string{"pool", "team"})
	must(err)
	if len(us) != 2 || us[0].Team != "etabA" || us[0].Pool != "p" || us[0].Requests != 2 || us[0].PromptTokens != 11 ||
		us[0].CompletionTokens != 6 || us[0].CachedTokens != 2 || us[0].GPUHours != 2 || us[1].GPUHours != 0.25 ||
		us[0].EstimatedRequests != 1 || us[1].EstimatedRequests != 0 || us[0].Org != "" {
		t.Fatalf("report by team and pool %+v", us)
	}
	// By tag: the record with two tags counts under both, untagged under "".
	us, err = s.Report(ctx, t0, t0.Add(72*time.Hour), []string{"tag"})
	must(err)
	if len(us) != 3 || us[0].Tag != "" || us[0].Requests != 2 || us[1].Tag != "eval" || us[1].Requests != 2 ||
		us[2].Tag != "prod" || us[2].GPUHours != 0.5 {
		t.Fatalf("report by tag %+v", us)
	}
	// Grand total, and by org and project.
	us, err = s.Report(ctx, t0, t0.Add(72*time.Hour), nil)
	must(err)
	if len(us) != 1 || us[0].Requests != 4 || us[0].GPUHours != 9.25 {
		t.Fatalf("grand total %+v", us)
	}
	us, err = s.Report(ctx, t0, t0.Add(72*time.Hour), []string{"project", "org"})
	must(err)
	if len(us) != 3 || us[0].Org != "orgA" || us[2].Project != "x" || us[2].GPUHours != 7 {
		t.Fatalf("by org and project %+v", us)
	}
	// By UTC day, with tags (the day survives the tag expansion; rows are
	// ordered by tag, then day).
	us, err = s.Report(ctx, t0, t0.Add(72*time.Hour), []string{"day", "tag"})
	must(err)
	if len(us) != 4 || us[0].Day != "2026-10-01" || us[0].Tag != "" || us[1].Day != "2026-10-03" || us[1].GPUHours != 7 ||
		us[2].Tag != "eval" || us[2].Day != "2026-10-01" || us[2].GPUHours != 2 {
		t.Fatalf("by day %+v", us)
	}
	if us, err := s.Report(ctx, t0.AddDate(1, 0, 0), t0.AddDate(1, 0, 1), nil); err != nil || len(us) != 0 {
		t.Fatalf("empty period %+v %v", us, err)
	}
	if _, err := s.Report(ctx, t0, t0, []string{"planet"}); err == nil {
		t.Fatal("unknown dimension accepted")
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
	must(s.CreateTeam(ctx, "etabB", "orgB")) // re-enable
	must(s.DeleteBudget(ctx, teamB))
	if err := s.DeleteBudget(ctx, teamB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing budget: %v", err)
	}

	// Disabling an organisation disables its teams and revokes their keys.
	_, id3, h3 := GenerateKey()
	must(s.CreateKey(ctx, Key{ID: id3, Hash: h3, Team: "etabB"}))
	must(s.DisableOrg(ctx, "orgB"))
	if k, _ := s.KeyByHash(ctx, h3); !k.Revoked {
		t.Fatal("disabling an org must revoke its keys")
	}
	if tm, _ := s.GetTeam(ctx, "etabB"); !tm.Disabled {
		t.Fatal("disabling an org must disable its teams")
	}
	if err := s.CreateTeam(ctx, "etabC", "orgB"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("team in disabled org: %v", err)
	}
	if err := s.DisableOrg(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disable unknown org: %v", err)
	}
	must(s.CreateOrg(ctx, "orgB")) // re-enable
}

func TestMemoryContract(t *testing.T) {
	m := NewMemory()
	contract(t, m)
	contractUsers(t, m)
	contractAudit(t, m)
	m.Close()
}

// contractAudit: appends are numbered and chained without gaps, even from
// concurrent writers (two replicas), checkpoints are unique per seq, prune
// removes a prefix, and a log emptied by a prune continues from its newest
// checkpoint.
func contractAudit(t *testing.T, s Store) {
	ctx := context.Background()
	hasher := func(e *AuditEntry) []byte {
		h := sha256.Sum256(append(append([]byte{}, e.PrevHash...), []byte(fmt.Sprint(e.Seq, e.Action))...))
		return h[:]
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.AuditAppend(ctx, AuditEntry{Time: time.Now(), Kind: "admin", Actor: "a", Roles: "[]", Action: fmt.Sprint("act", i),
				Before: "null", After: "{}", Status: 200}, hasher); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	es, err := s.AuditRange(ctx, 1, 100)
	if err != nil || len(es) != 20 {
		t.Fatalf("audit entries: %d %v", len(es), err)
	}
	prev := make([]byte, 32)
	for i, e := range es {
		if e.Seq != int64(i+1) || string(e.PrevHash) != string(prev) || string(e.Hash) != string(hasher(&e)) || e.Time.Location() != time.UTC {
			t.Fatalf("entry %d: %+v", i, e)
		}
		prev = e.Hash
	}
	if es, _ := s.AuditRange(ctx, 5, 3); len(es) != 3 || es[0].Seq != 5 {
		t.Fatalf("range: %+v", es)
	}
	cp := AuditCheckpoint{Seq: 10, Hash: es[0].Hash, Time: time.Now(), KeyID: "k", Signature: []byte("sig")}
	if err := s.AuditAddCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if err := s.AuditAddCheckpoint(ctx, cp); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate checkpoint: %v", err)
	}
	cp.Seq, cp.Hash = 20, prev
	if err := s.AuditAddCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if cps, err := s.AuditCheckpoints(ctx); err != nil || len(cps) != 2 || cps[0].Seq != 10 || string(cps[1].Signature) != "sig" {
		t.Fatalf("checkpoints: %+v %v", cps, err)
	}
	if n, err := s.AuditPrune(ctx, 20); err != nil || n != 20 {
		t.Fatalf("prune: %d %v", n, err)
	}
	e, err := s.AuditAppend(ctx, AuditEntry{Time: time.Now(), Kind: "admin", Roles: "[]", Before: "null", After: "{}"}, hasher)
	if err != nil || e.Seq != 21 || string(e.PrevHash) != string(prev) {
		t.Fatalf("append after an emptying prune: %+v %v", e, err)
	}
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
		m.Ping(ctx), m.CreateOrg(ctx, "o"), m.DisableOrg(ctx, "o"), m.CreateTeam(ctx, "a", "o"), m.DisableTeam(ctx, "a"),
		m.CreateProject(ctx, "a", "p"), m.DisableProject(ctx, "a", "p"), m.CreateKey(ctx, Key{}),
		m.RevokeKey(ctx, "a"), m.SetBudget(ctx, Budget{BudgetKey: BudgetKey{Kind: ScopeTag, Name: "a"}, Period: Day}),
		m.DeleteBudget(ctx, BudgetKey{}), m.IncreaseBudget(ctx, BudgetKey{}, 1, time.Time{}),
		m.InsertRecords(ctx, nil),
	}
	_, e1 := m.ListTeams(ctx, "")
	_, e2 := m.ListKeys(ctx, "")
	_, e3 := m.KeyByHash(ctx, [32]byte{})
	_, e4 := m.Budgets(ctx)
	_, e5 := m.SpendRows(ctx, time.Time{})
	if _, err := m.MarkSoftAlert(ctx, BudgetKey{}, time.Time{}); !errors.Is(err, boom) {
		t.Fatal("MarkSoftAlert fault")
	}
	_, e6 := m.Report(ctx, time.Time{}, time.Time{}, nil)
	_, e7 := m.ListOrgs(ctx)
	_, e8 := m.GetTeam(ctx, "a")
	_, e9 := m.ListProjects(ctx, "a")
	_, e10 := m.GetKey(ctx, "a")
	for i, err := range append(checks, e1, e2, e3, e4, e5, e6, e7, e8, e9, e10) {
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
	dropAll(t, p)
	p.Close()
	p, err = OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	contract(t, p)
	contractUsers(t, p)
	contractModels(t, p)
	contractTools(t, p)
	contractAudit(t, p)
	// The table refuses an UPDATE: rewriting history needs more than SQL
	// access to the gateway's own role.
	if _, err := p.pool.Exec(ctx, `UPDATE llmflow_audit SET actor = 'mallory'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("audit UPDATE: %v", err)
	}
	contractAllowlists(t, p)
	contractUnits(t, p)
	contractLedger(t, p)
	contractArming(t, p)
	contractRolling(t, p)
	contractRollingWindows(t, func() Store { return p }, 20)
	contractProfiles(t, p)

	if _, err := OpenPostgres(ctx, "postgres://u:hunter2@[::1"); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("bad DSN error leaks or passes: %v", err)
	}
}

func dropAll(t *testing.T, p *Postgres) {
	t.Helper()
	if _, err := p.pool.Exec(context.Background(),
		`DROP TABLE IF EXISTS llmflow_pool_capacity, llmflow_energy_active, llmflow_energy_intervals, llmflow_budget_buckets, llmflow_profile_assignments, llmflow_workdays, llmflow_budget_ledger, llmflow_budget_grants, llmflow_budget_holders, llmflow_budget_arming,llmflow_audit, llmflow_audit_checkpoints, llmflow_scim,llmflow_allowlists,llmflow_budgets_v2, llmflow_team_models, llmflow_team_tools,llmflow_bindings, llmflow_user_tokens, llmflow_users,
		 llmflow_usage, llmflow_budgets, llmflow_keys, llmflow_projects, llmflow_teams, llmflow_orgs`); err != nil {
		t.Fatal(err)
	}
}

// A v0.1.0 database (teams without organisation, keys without project,
// usage without org/project/estimated) is upgraded in place, and its teams
// are adopted by the organisation the configuration puts them in.
func TestPostgresUpgradeFromV010(t *testing.T) {
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
	h := HashSecret("old-key")
	for _, q := range []string{
		`CREATE TABLE llmflow_teams (name text PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now(), disabled boolean NOT NULL DEFAULT false)`,
		`CREATE TABLE llmflow_keys (id text PRIMARY KEY, hash bytea NOT NULL UNIQUE, team text NOT NULL REFERENCES llmflow_teams(name),
			name text NOT NULL DEFAULT '', rpm integer NOT NULL DEFAULT 0, tpm integer NOT NULL DEFAULT 0,
			max_inflight integer NOT NULL DEFAULT 0, created_at timestamptz NOT NULL DEFAULT now(), revoked boolean NOT NULL DEFAULT false)`,
		`CREATE TABLE llmflow_usage (id bigserial PRIMARY KEY, ts timestamptz NOT NULL, team text NOT NULL, key_id text NOT NULL,
			model text NOT NULL, pool text NOT NULL, upstream text NOT NULL, prompt_tokens bigint NOT NULL, completion_tokens bigint NOT NULL,
			cached_tokens bigint NOT NULL, cost double precision NOT NULL, latency_ms double precision NOT NULL, status integer NOT NULL,
			priority integer NOT NULL, borrowing boolean NOT NULL, stream boolean NOT NULL)`,
		`INSERT INTO llmflow_teams (name) VALUES ('etabA')`,
	} {
		if _, err := p.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.pool.Exec(ctx, `INSERT INTO llmflow_keys (id, hash, team) VALUES ('k_old', $1, 'etabA')`, h[:]); err != nil {
		t.Fatal(err)
	}
	p.Close()
	p, err = OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer p.Close()
	if err := p.CreateOrg(ctx, "orgA"); err != nil {
		t.Fatal(err)
	}
	if err := p.CreateTeam(ctx, "etabA", "orgA"); err != nil {
		t.Fatalf("adopt v0.1 team: %v", err)
	}
	k, err := p.KeyByHash(ctx, h)
	if err != nil || k.Org != "orgA" || k.Project != "" || k.Revoked {
		t.Fatalf("old key after upgrade: %+v %v", k, err)
	}
	if err := p.InsertRecords(ctx, []Record{{Time: time.Now(), Org: "orgA", Team: "etabA", KeyID: "k_old", Estimated: true}}); err != nil {
		t.Fatalf("insert after upgrade: %v", err)
	}
}
