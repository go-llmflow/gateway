package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Memory is an in-process Store. Usage records are bounded by MaxRecords
// (oldest dropped first).
type Memory struct {
	mu         sync.RWMutex
	orgs       map[string]*Org
	teams      map[string]*Team
	projects   map[[2]string]*Project
	keys       map[string]*Key
	byHash     map[[32]byte]string
	budgets    map[BudgetKey]*memBudget
	records    []Record
	u          memUsers
	teamModels map[string][]string
	MaxRecords int
	// Fail, when set, is returned by every call (fault injection). Set it
	// with SetFail.
	Fail error
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		orgs: map[string]*Org{}, teams: map[string]*Team{}, projects: map[[2]string]*Project{},
		keys: map[string]*Key{}, byHash: map[[32]byte]string{}, budgets: map[BudgetKey]*memBudget{},
		MaxRecords: 1_000_000,
	}
}

// SetFail makes every later call return err (nil restores the store).
func (m *Memory) SetFail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Fail = err
}

func (m *Memory) Ping(context.Context) error { m.mu.RLock(); defer m.mu.RUnlock(); return m.Fail }

func now() time.Time { return time.Now().UTC() }

func (m *Memory) CreateOrg(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if o, ok := m.orgs[name]; ok {
		if o.Disabled {
			o.Disabled = false
			return nil
		}
		return ErrExists
	}
	m.orgs[name] = &Org{Name: name, CreatedAt: now()}
	return nil
}

func (m *Memory) ListOrgs(context.Context) ([]Org, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := make([]Org, 0, len(m.orgs))
	for _, o := range m.orgs {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) DisableOrg(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	o, ok := m.orgs[name]
	if !ok {
		return ErrNotFound
	}
	o.Disabled = true
	for _, t := range m.teams {
		if t.Org == name {
			m.disableTeamLocked(t)
		}
	}
	return nil
}

func (m *Memory) disableTeamLocked(t *Team) {
	t.Disabled = true
	for _, p := range m.projects {
		if p.Team == t.Name {
			p.Disabled = true
		}
	}
	for _, k := range m.keys {
		if k.Team == t.Name {
			k.Revoked = true
		}
	}
}

func (m *Memory) CreateTeam(_ context.Context, name, org string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	o, ok := m.orgs[org]
	if !ok || o.Disabled {
		return ErrNotFound
	}
	if t, ok := m.teams[name]; ok {
		switch {
		case t.Org == "":
			t.Org, t.Disabled = org, false
			return nil
		case t.Org != org:
			return ErrConflict
		case t.Disabled:
			t.Disabled = false
			return nil
		}
		return ErrExists
	}
	m.teams[name] = &Team{Name: name, Org: org, CreatedAt: now()}
	return nil
}

func (m *Memory) GetTeam(_ context.Context, name string) (Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return Team{}, m.Fail
	}
	t, ok := m.teams[name]
	if !ok {
		return Team{}, ErrNotFound
	}
	return *t, nil
}

func (m *Memory) ListTeams(_ context.Context, org string) ([]Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := make([]Team, 0, len(m.teams))
	for _, t := range m.teams {
		if org == "" || t.Org == org {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) DisableTeam(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	t, ok := m.teams[name]
	if !ok {
		return ErrNotFound
	}
	m.disableTeamLocked(t)
	return nil
}

func (m *Memory) CreateProject(_ context.Context, team, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	t, ok := m.teams[team]
	if !ok || t.Disabled {
		return ErrNotFound
	}
	k := [2]string{team, name}
	if p, ok := m.projects[k]; ok {
		if p.Disabled {
			p.Disabled = false
			return nil
		}
		return ErrExists
	}
	m.projects[k] = &Project{Team: team, Name: name, CreatedAt: now()}
	return nil
}

func (m *Memory) ListProjects(_ context.Context, team string) ([]Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := []Project{}
	for _, p := range m.projects {
		if p.Team == team {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) DisableProject(_ context.Context, team, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	p, ok := m.projects[[2]string{team, name}]
	if !ok {
		return ErrNotFound
	}
	p.Disabled = true
	for _, k := range m.keys {
		if k.Team == team && k.Project == name {
			k.Revoked = true
		}
	}
	return nil
}

func (m *Memory) CreateKey(_ context.Context, k Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	t, ok := m.teams[k.Team]
	if !ok || t.Disabled {
		return ErrNotFound
	}
	if k.Project != "" {
		p, ok := m.projects[[2]string{k.Team, k.Project}]
		if !ok || p.Disabled {
			return ErrNotFound
		}
	}
	if _, ok := m.keys[k.ID]; ok {
		return ErrExists
	}
	if _, ok := m.byHash[k.Hash]; ok {
		return ErrExists
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = now()
	}
	k.Org = ""
	if len(k.Tags) > 0 {
		k.Tags = dedupSorted(k.Tags)
	} else {
		k.Tags = nil
	}
	m.keys[k.ID] = &k
	m.byHash[k.Hash] = k.ID
	return nil
}

// withOrg returns a copy of k with its team's organisation.
func (m *Memory) withOrg(k *Key) Key {
	out := *k
	if t, ok := m.teams[k.Team]; ok {
		out.Org = t.Org
	}
	return out
}

func (m *Memory) ListKeys(_ context.Context, team string) ([]Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := []Key{}
	for _, k := range m.keys {
		if team == "" || k.Team == team {
			out = append(out, m.withOrg(k))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) GetKey(_ context.Context, id string) (Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return Key{}, m.Fail
	}
	k, ok := m.keys[id]
	if !ok {
		return Key{}, ErrNotFound
	}
	return m.withOrg(k), nil
}

func (m *Memory) RevokeKey(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	k, ok := m.keys[id]
	if !ok {
		return ErrNotFound
	}
	k.Revoked = true
	return nil
}

func (m *Memory) KeyByHash(_ context.Context, h [32]byte) (Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return Key{}, m.Fail
	}
	id, ok := m.byHash[h]
	if !ok {
		return Key{}, ErrNotFound
	}
	return m.withOrg(m.keys[id]), nil
}

func (m *Memory) InsertRecords(_ context.Context, recs []Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.records = append(m.records, recs...)
	if over := len(m.records) - m.MaxRecords; m.MaxRecords > 0 && over > 0 {
		m.records = append(m.records[:0:0], m.records[over:]...)
	}
	return nil
}

func (m *Memory) Usage(_ context.Context, from, to time.Time) ([]TeamUsage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	agg := map[[2]string]*TeamUsage{}
	for _, r := range m.records {
		if r.Time.Before(from) || !r.Time.Before(to) {
			continue
		}
		k := [2]string{r.Team, r.Pool}
		u := agg[k]
		if u == nil {
			u = &TeamUsage{Team: r.Team, Pool: r.Pool}
			agg[k] = u
		}
		u.Requests++
		u.PromptTokens += r.PromptTokens
		u.CompletionTokens += r.CompletionTokens
		u.CachedTokens += r.CachedTokens
		u.GPUHours += r.Cost
		if r.Estimated {
			u.EstimatedRequests++
		}
	}
	out := make([]TeamUsage, 0, len(agg))
	for _, u := range agg {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Team != out[j].Team {
			return out[i].Team < out[j].Team
		}
		return out[i].Pool < out[j].Pool
	})
	return out, nil
}

// Records returns a copy of the stored usage records (tests, trials).
func (m *Memory) Records() []Record {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Record(nil), m.records...)
}

func (m *Memory) Close() {}
