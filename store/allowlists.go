package store

import (
	"context"
	"fmt"
	"net/netip"
	"sort"

	"github.com/jackc/pgx/v5"
)

// AllowKey names what an IP allowlist applies to: an organisation, a team
// or a key (by ID).
type AllowKey struct {
	Kind ScopeKind `json:"kind"` // ScopeOrg, ScopeTeam or ScopeKey
	Name string    `json:"name"`
}

// Allowlist is the set of client networks allowed for one scope.
type Allowlist struct {
	AllowKey
	CIDRs []string `json:"cidrs"`
}

const maxCIDRs = 256

// ValidateAllowlist checks the kind and canonicalises the prefixes.
func ValidateAllowlist(a Allowlist) (Allowlist, error) {
	switch a.Kind {
	case ScopeOrg, ScopeTeam, ScopeKey:
	default:
		return a, fmt.Errorf("kind %q: want org, team or key", a.Kind)
	}
	if a.Name == "" || len(a.Name) > 140 {
		return a, fmt.Errorf("name %q is invalid", a.Name)
	}
	if len(a.CIDRs) > maxCIDRs {
		return a, fmt.Errorf("at most %d prefixes", maxCIDRs)
	}
	out := make([]string, 0, len(a.CIDRs))
	for _, c := range a.CIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return a, fmt.Errorf("prefix %q: %v", c, err)
		}
		out = append(out, p.Masked().String())
	}
	a.CIDRs = dedupSorted(out)
	return a, nil
}

func (m *Memory) SetAllowlist(_ context.Context, a Allowlist) error {
	a, err := ValidateAllowlist(a)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if m.allow == nil {
		m.allow = map[AllowKey][]string{}
	}
	if len(a.CIDRs) == 0 {
		delete(m.allow, a.AllowKey)
		return nil
	}
	m.allow[a.AllowKey] = a.CIDRs
	return nil
}

func (m *Memory) Allowlists(context.Context) ([]Allowlist, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := make([]Allowlist, 0, len(m.allow))
	for k, c := range m.allow {
		out = append(out, Allowlist{AllowKey: k, CIDRs: append([]string(nil), c...)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

const allowSchema = `
CREATE TABLE IF NOT EXISTS llmflow_allowlists (
	kind  text NOT NULL,
	name  text NOT NULL,
	cidrs text[] NOT NULL,
	PRIMARY KEY (kind, name)
);
`

func (p *Postgres) SetAllowlist(ctx context.Context, a Allowlist) error {
	a, err := ValidateAllowlist(a)
	if err != nil {
		return err
	}
	if len(a.CIDRs) == 0 {
		_, err := p.pool.Exec(ctx, `DELETE FROM llmflow_allowlists WHERE kind = $1 AND name = $2`, string(a.Kind), a.Name)
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO llmflow_allowlists (kind, name, cidrs) VALUES ($1, $2, $3)
		ON CONFLICT (kind, name) DO UPDATE SET cidrs = EXCLUDED.cidrs`, string(a.Kind), a.Name, a.CIDRs)
	return err
}

func (p *Postgres) Allowlists(ctx context.Context) ([]Allowlist, error) {
	rows, err := p.pool.Query(ctx, `SELECT kind, name, cidrs FROM llmflow_allowlists ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Allowlist, error) {
		var a Allowlist
		var kind string
		err := r.Scan(&kind, &a.Name, &a.CIDRs)
		a.Kind = ScopeKind(kind)
		return a, err
	})
}
