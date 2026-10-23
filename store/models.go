package store

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5"
)

const modelsSchema = `
CREATE TABLE IF NOT EXISTS llmflow_team_models (
	team  text NOT NULL REFERENCES llmflow_teams(name),
	model text NOT NULL,
	PRIMARY KEY (team, model)
);
`

func dedupSorted(models []string) []string {
	out := append([]string(nil), models...)
	sort.Strings(out)
	n := 0
	for i, m := range out {
		if i == 0 || m != out[i-1] {
			out[n] = m
			n++
		}
	}
	return out[:n]
}

func (m *Memory) SetTeamModels(_ context.Context, team string, models []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.teams[team]; !ok {
		return ErrNotFound
	}
	if m.teamModels == nil {
		m.teamModels = map[string][]string{}
	}
	if len(models) == 0 {
		delete(m.teamModels, team)
		return nil
	}
	m.teamModels[team] = dedupSorted(models)
	return nil
}

func (m *Memory) TeamModels(context.Context) (map[string][]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := map[string][]string{}
	for t, ms := range m.teamModels {
		out[t] = append([]string(nil), ms...)
	}
	return out, nil
}

func (p *Postgres) SetTeamModels(ctx context.Context, team string, models []string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llmflow_teams WHERE name = $1)`, team).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM llmflow_team_models WHERE team = $1`, team); err != nil {
			return err
		}
		for _, m := range dedupSorted(models) {
			if _, err := tx.Exec(ctx, `INSERT INTO llmflow_team_models (team, model) VALUES ($1, $2)`, team, m); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *Postgres) TeamModels(ctx context.Context) (map[string][]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT team, model FROM llmflow_team_models ORDER BY team, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var t, m string
		if err := rows.Scan(&t, &m); err != nil {
			return nil, err
		}
		out[t] = append(out[t], m)
	}
	return out, rows.Err()
}
