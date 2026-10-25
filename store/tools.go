package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// The MCP tools a team may call: patterns over "<server>.<tool>". Unlike
// models, no list means no tool.

const toolsSchema = `
CREATE TABLE IF NOT EXISTS llmflow_team_tools (
	team    text NOT NULL REFERENCES llmflow_teams(name),
	pattern text NOT NULL,
	PRIMARY KEY (team, pattern)
);
`

func (m *Memory) SetTeamTools(_ context.Context, team string, tools []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.teams[team]; !ok {
		return ErrNotFound
	}
	if m.teamTools == nil {
		m.teamTools = map[string][]string{}
	}
	if len(tools) == 0 {
		delete(m.teamTools, team)
		return nil
	}
	m.teamTools[team] = dedupSorted(tools)
	return nil
}

func (m *Memory) TeamTools(context.Context) (map[string][]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := map[string][]string{}
	for t, ts := range m.teamTools {
		out[t] = append([]string(nil), ts...)
	}
	return out, nil
}

func (p *Postgres) SetTeamTools(ctx context.Context, team string, tools []string) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llmflow_teams WHERE name = $1)`, team).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM llmflow_team_tools WHERE team = $1`, team); err != nil {
			return err
		}
		for _, t := range dedupSorted(tools) {
			if _, err := tx.Exec(ctx, `INSERT INTO llmflow_team_tools (team, pattern) VALUES ($1, $2)`, team, t); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *Postgres) TeamTools(ctx context.Context) (map[string][]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT team, pattern FROM llmflow_team_tools ORDER BY team, pattern`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var t, pat string
		if err := rows.Scan(&t, &pat); err != nil {
			return nil, err
		}
		out[t] = append(out[t], pat)
	}
	return out, rows.Err()
}
