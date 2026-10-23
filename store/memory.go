package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Memory is an in-process Store. Usage records are kept in memory without
// bound on their number other than MaxRecords (oldest dropped first).
type Memory struct {
	mu         sync.RWMutex
	teams      map[string]*Team
	keys       map[string]*Key
	byHash     map[[32]byte]string
	budgets    map[string]Budget
	records    []Record
	MaxRecords int
	// Fail, when set, is returned by every call (fault injection). Set it
	// with SetFail.
	Fail error
}

// SetFail makes every later call return err (nil restores the store).
func (m *Memory) SetFail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Fail = err
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		teams: map[string]*Team{}, keys: map[string]*Key{}, byHash: map[[32]byte]string{},
		budgets: map[string]Budget{}, MaxRecords: 1_000_000,
	}
}

func (m *Memory) Ping(context.Context) error { m.mu.RLock(); defer m.mu.RUnlock(); return m.Fail }

func (m *Memory) CreateTeam(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if t, ok := m.teams[name]; ok {
		if t.Disabled {
			t.Disabled = false
			return nil
		}
		return ErrExists
	}
	m.teams[name] = &Team{Name: name, CreatedAt: time.Now().UTC()}
	return nil
}

func (m *Memory) ListTeams(context.Context) ([]Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := make([]Team, 0, len(m.teams))
	for _, t := range m.teams {
		out = append(out, *t)
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
	t.Disabled = true
	for _, k := range m.keys {
		if k.Team == name {
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
	if _, ok := m.keys[k.ID]; ok {
		return ErrExists
	}
	if _, ok := m.byHash[k.Hash]; ok {
		return ErrExists
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	m.keys[k.ID] = &k
	m.byHash[k.Hash] = k.ID
	return nil
}

func (m *Memory) ListKeys(_ context.Context, team string) ([]Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	var out []Key
	for _, k := range m.keys {
		if team == "" || k.Team == team {
			out = append(out, *k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
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
	return *m.keys[id], nil
}

func (m *Memory) SetBudget(_ context.Context, b Budget) error {
	if err := ValidateBudget(b); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.teams[b.Team]; !ok {
		return ErrNotFound
	}
	m.budgets[b.Team] = b
	return nil
}

func (m *Memory) DeleteBudget(_ context.Context, team string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.budgets[team]; !ok {
		return ErrNotFound
	}
	delete(m.budgets, team)
	return nil
}

func (m *Memory) Budgets(context.Context) ([]Budget, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := make([]Budget, 0, len(m.budgets))
	for _, b := range m.budgets {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Team < out[j].Team })
	return out, nil
}

func (m *Memory) SpendSince(_ context.Context, since time.Time) (map[string]float64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := map[string]float64{}
	for _, r := range m.records {
		if !r.Time.Before(since) {
			out[r.Team] += r.Cost
		}
	}
	return out, nil
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

func (m *Memory) Close() {}
