package store

import (
	"context"
	"sort"
)

type memUsers struct {
	users       map[string]*User
	tokens      map[string]*UserToken
	tokenByHash map[[32]byte]string
	bindings    map[Binding]Provenance // keyed by Binding.Key()
}

func (m *Memory) usersInit() {
	if m.u.users == nil {
		m.u = memUsers{users: map[string]*User{}, tokens: map[string]*UserToken{},
			tokenByHash: map[[32]byte]string{}, bindings: map[Binding]Provenance{}}
	}
}

func (m *Memory) CreateUser(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.usersInit()
	if u, ok := m.u.users[name]; ok {
		if u.Disabled {
			u.Disabled = false
			return nil
		}
		return ErrExists
	}
	m.u.users[name] = &User{Name: name, CreatedAt: now()}
	return nil
}

func (m *Memory) GetUser(_ context.Context, name string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return User{}, m.Fail
	}
	m.usersInit()
	u, ok := m.u.users[name]
	if !ok {
		return User{}, ErrNotFound
	}
	return *u, nil
}

func (m *Memory) ListUsers(context.Context) ([]User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	m.usersInit()
	out := []User{}
	for _, u := range m.u.users {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) DisableUser(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.usersInit()
	u, ok := m.u.users[name]
	if !ok {
		return ErrNotFound
	}
	u.Disabled = true
	for _, t := range m.u.tokens {
		if t.User == name {
			t.Revoked = true
		}
	}
	return nil
}

func (m *Memory) CreateUserToken(_ context.Context, t UserToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.usersInit()
	u, ok := m.u.users[t.User]
	if !ok || u.Disabled {
		return ErrNotFound
	}
	if _, ok := m.u.tokens[t.ID]; ok {
		return ErrExists
	}
	if _, ok := m.u.tokenByHash[t.Hash]; ok {
		return ErrExists
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now()
	}
	m.u.tokens[t.ID] = &t
	m.u.tokenByHash[t.Hash] = t.ID
	return nil
}

func (m *Memory) ListUserTokens(_ context.Context, user string) ([]UserToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	m.usersInit()
	out := []UserToken{}
	for _, t := range m.u.tokens {
		if t.User == user {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) RevokeUserToken(_ context.Context, user, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.usersInit()
	t, ok := m.u.tokens[id]
	if !ok || t.User != user {
		return ErrNotFound
	}
	t.Revoked = true
	return nil
}

func (m *Memory) UserByTokenHash(_ context.Context, h [32]byte) (User, UserToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return User{}, UserToken{}, m.Fail
	}
	m.usersInit()
	id, ok := m.u.tokenByHash[h]
	if !ok {
		return User{}, UserToken{}, ErrNotFound
	}
	t := m.u.tokens[id]
	return *m.u.users[t.User], *t, nil
}

func (m *Memory) Grant(_ context.Context, b Binding) error {
	if err := b.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.usersInit()
	if _, ok := m.u.users[b.User]; !ok {
		return ErrNotFound
	}
	if _, ok := m.orgs[b.Org]; b.Org != "" && !ok {
		return ErrNotFound
	}
	if _, ok := m.teams[b.Team]; b.Team != "" && !ok {
		return ErrNotFound
	}
	if _, ok := m.u.bindings[b.Key()]; ok {
		return ErrExists
	}
	m.u.bindings[b.Key()] = b.Provenance
	return nil
}

func (m *Memory) Ungrant(_ context.Context, b Binding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.usersInit()
	if _, ok := m.u.bindings[b.Key()]; !ok {
		return ErrNotFound
	}
	delete(m.u.bindings, b.Key())
	return nil
}

func (m *Memory) Bindings(_ context.Context, user string) ([]Binding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	m.usersInit()
	out := []Binding{}
	for b, prov := range m.u.bindings {
		if user == "" || b.User == user {
			b.Provenance = prov
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, c := out[i], out[j]
		if a.User != c.User {
			return a.User < c.User
		}
		if a.Role != c.Role {
			return a.Role < c.Role
		}
		if a.Org != c.Org {
			return a.Org < c.Org
		}
		return a.Team < c.Team
	})
	return out, nil
}
