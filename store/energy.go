package store

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/go-llmflow/gateway/energy"
	"github.com/jackc/pgx/v5"
)

// Measured energy (#97). An EnergyInterval is what one upstream's GPUs used
// between two scrapes; its active part is stored split between the consumers
// that did work in it, its idle part (and any active energy nobody could be
// attributed) stays whole, to be allocated by the report according to the
// agreement (energy.AllocateIdle). Everything is in millijoules.

// EnergyInterval is one stored interval.
type EnergyInterval struct {
	Upstream   string
	Start, End time.Time
	// TotalMJ is the measured energy; IdleMJ the part not attributed to a
	// consumer. TotalMJ - IdleMJ is the sum of the interval's active rows.
	TotalMJ, IdleMJ int64
	GPUs, Resets    int
	Gap             bool
}

// EnergyCell is the active energy of a consumer on a UTC day.
type EnergyCell struct {
	Day string
	energy.Consumer
	MJ int64
}

// EnergyIdle is the unattributed energy of a UTC day.
type EnergyIdle struct {
	Day string
	MJ  int64
}

// EnergyStore keeps measured energy. Memory and Postgres implement it.
type EnergyStore interface {
	// WorkBetween sums the work of the GPU-hour requests the upstreams (as
	// the records name them) finished in [from, to), per consumer.
	WorkBetween(ctx context.Context, upstreams []string, from, to time.Time) ([]energy.Work, error)
	// SaveEnergy stores an interval with its attributed rows, atomically.
	// ErrConflict when it overlaps an interval already stored for the
	// upstream (two replicas scraping the same node: the first wins).
	SaveEnergy(ctx context.Context, iv EnergyInterval, active []energy.Attributed) error
	// EnergyBetween returns the energy of the intervals that start in
	// [from, to), by day.
	EnergyBetween(ctx context.Context, from, to time.Time) ([]EnergyCell, []EnergyIdle, error)
	// LastEnergy is the end of the newest interval of an upstream (zero when none).
	LastEnergy(ctx context.Context, upstream string) (time.Time, error)
}

type memEnergy struct {
	ivs   []EnergyInterval
	cells []memCell
}

type memCell struct {
	upstream string
	start    time.Time
	energy.Attributed
}

func sortWork(w []energy.Work) {
	sort.Slice(w, func(i, j int) bool {
		a, b := w[i].Consumer, w[j].Consumer
		if a.Org != b.Org {
			return a.Org < b.Org
		}
		if a.Lab != b.Lab {
			return a.Lab < b.Lab
		}
		if a.Team != b.Team {
			return a.Team < b.Team
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.User != b.User {
			return a.User < b.User
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Pool < b.Pool
	})
}

// work converts a record's tokens to prefill and decode tokens: a cached
// prompt token was read from the prefix cache, not computed.
func workOf(r Record) energy.Work {
	return energy.Work{
		Consumer: energy.Consumer{Org: r.Org, Lab: r.Lab, Team: r.Team, Project: r.Project, User: r.User, Model: r.Model, Pool: r.Pool},
		Prefill:  max(r.PromptTokens-r.CachedTokens, 0), Decode: max(r.CompletionTokens, 0),
	}
}

func (m *Memory) WorkBetween(ctx context.Context, upstreams []string, from, to time.Time) ([]energy.Work, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	agg := map[energy.Consumer]*energy.Work{}
	for _, r := range m.records {
		if !slices.Contains(upstreams, r.Upstream) || r.Unit != "" || r.Time.Before(from) || !r.Time.Before(to) {
			continue
		}
		w := workOf(r)
		if a := agg[w.Consumer]; a != nil {
			a.Prefill += w.Prefill
			a.Decode += w.Decode
		} else {
			agg[w.Consumer] = &w
		}
	}
	out := make([]energy.Work, 0, len(agg))
	for _, w := range agg {
		out = append(out, *w)
	}
	sortWork(out)
	return out, nil
}

func overlaps(a0, a1, b0, b1 time.Time) bool { return a0.Before(b1) && b0.Before(a1) }

func (m *Memory) SaveEnergy(ctx context.Context, iv EnergyInterval, active []energy.Attributed) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	for _, o := range m.en.ivs {
		if o.Upstream == iv.Upstream && overlaps(o.Start, o.End, iv.Start, iv.End) {
			return ErrConflict
		}
	}
	m.en.ivs = append(m.en.ivs, iv)
	for _, a := range active {
		m.en.cells = append(m.en.cells, memCell{iv.Upstream, iv.Start, a})
	}
	return nil
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (m *Memory) EnergyBetween(ctx context.Context, from, to time.Time) ([]EnergyCell, []EnergyIdle, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, nil, m.Fail
	}
	type ck struct {
		d string
		c energy.Consumer
	}
	cells := map[ck]int64{}
	for _, c := range m.en.cells {
		if c.start.Before(from) || !c.start.Before(to) {
			continue
		}
		cells[ck{day(c.start), c.Consumer}] += c.MJ
	}
	idle := map[string]int64{}
	for _, iv := range m.en.ivs {
		if iv.Start.Before(from) || !iv.Start.Before(to) {
			continue
		}
		idle[day(iv.Start)] += iv.IdleMJ
	}
	var oc []EnergyCell
	for k, v := range cells {
		oc = append(oc, EnergyCell{k.d, k.c, v})
	}
	sort.Slice(oc, func(i, j int) bool {
		if oc[i].Day != oc[j].Day {
			return oc[i].Day < oc[j].Day
		}
		return oc[i].Consumer.Org+"\x00"+oc[i].User+"\x00"+oc[i].Team+"\x00"+oc[i].Project+"\x00"+oc[i].Model <
			oc[j].Consumer.Org+"\x00"+oc[j].User+"\x00"+oc[j].Team+"\x00"+oc[j].Project+"\x00"+oc[j].Model
	})
	return oc, sortIdle(idle), nil
}

func sortIdle(m map[string]int64) []EnergyIdle {
	out := make([]EnergyIdle, 0, len(m))
	for d, v := range m {
		out = append(out, EnergyIdle{d, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

func (m *Memory) LastEnergy(ctx context.Context, upstream string) (time.Time, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var last time.Time
	for _, iv := range m.en.ivs {
		if iv.Upstream == upstream && iv.End.After(last) {
			last = iv.End
		}
	}
	return last, nil
}

const energySchema = `
CREATE TABLE IF NOT EXISTS llmflow_energy_intervals (
	upstream text NOT NULL,
	ts_start timestamptz NOT NULL,
	ts_end   timestamptz NOT NULL,
	total_mj bigint NOT NULL,
	idle_mj  bigint NOT NULL,
	gpus     integer NOT NULL DEFAULT 0,
	resets   integer NOT NULL DEFAULT 0,
	gap      boolean NOT NULL DEFAULT false,
	PRIMARY KEY (upstream, ts_start)
);
CREATE INDEX IF NOT EXISTS llmflow_energy_intervals_ts ON llmflow_energy_intervals (ts_start);
CREATE TABLE IF NOT EXISTS llmflow_energy_active (
	upstream  text NOT NULL,
	ts_start  timestamptz NOT NULL,
	org text NOT NULL, lab text NOT NULL, team text NOT NULL, project text NOT NULL,
	user_name text NOT NULL, model text NOT NULL, pool text NOT NULL,
	mj bigint NOT NULL,
	PRIMARY KEY (upstream, ts_start, org, lab, team, project, user_name, model, pool),
	FOREIGN KEY (upstream, ts_start) REFERENCES llmflow_energy_intervals (upstream, ts_start) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS llmflow_energy_active_ts ON llmflow_energy_active (ts_start);
`

func (p *Postgres) WorkBetween(ctx context.Context, upstreams []string, from, to time.Time) ([]energy.Work, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT org, lab, team, project, user_name, model, pool,
		       coalesce(sum(greatest(prompt_tokens - cached_tokens, 0)), 0), coalesce(sum(completion_tokens), 0)
		FROM llmflow_usage
		WHERE upstream = ANY($1) AND ts >= $2 AND ts < $3 AND unit = ''
		GROUP BY org, lab, team, project, user_name, model, pool`, upstreams, from, to)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (energy.Work, error) {
		var w energy.Work
		err := r.Scan(&w.Org, &w.Lab, &w.Team, &w.Project, &w.User, &w.Model, &w.Pool, &w.Prefill, &w.Decode)
		return w, err
	})
	sortWork(out)
	return out, err
}

func (p *Postgres) SaveEnergy(ctx context.Context, iv EnergyInterval, active []energy.Attributed) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('energy:' || $1, 0))`, iv.Upstream); err != nil {
			return err
		}
		var clash bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llmflow_energy_intervals WHERE upstream = $1 AND ts_start < $3 AND $2 < ts_end)`,
			iv.Upstream, iv.Start.UTC(), iv.End.UTC()).Scan(&clash); err != nil {
			return err
		}
		if clash {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `INSERT INTO llmflow_energy_intervals (upstream, ts_start, ts_end, total_mj, idle_mj, gpus, resets, gap) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			iv.Upstream, iv.Start.UTC(), iv.End.UTC(), iv.TotalMJ, iv.IdleMJ, iv.GPUs, iv.Resets, iv.Gap); err != nil {
			return err
		}
		for _, a := range active {
			if _, err := tx.Exec(ctx, `INSERT INTO llmflow_energy_active (upstream, ts_start, org, lab, team, project, user_name, model, pool, mj)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
				ON CONFLICT (upstream, ts_start, org, lab, team, project, user_name, model, pool) DO UPDATE SET mj = llmflow_energy_active.mj + EXCLUDED.mj`,
				iv.Upstream, iv.Start.UTC(), a.Org, a.Lab, a.Team, a.Project, a.User, a.Model, a.Pool, a.MJ); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *Postgres) EnergyBetween(ctx context.Context, from, to time.Time) ([]EnergyCell, []EnergyIdle, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT to_char(ts_start AT TIME ZONE 'UTC', 'YYYY-MM-DD'), org, lab, team, project, user_name, model, pool, sum(mj)::bigint
		FROM llmflow_energy_active WHERE ts_start >= $1 AND ts_start < $2
		GROUP BY 1, org, lab, team, project, user_name, model, pool ORDER BY 1, org, team, project, user_name, model`, from, to)
	if err != nil {
		return nil, nil, err
	}
	cells, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnergyCell, error) {
		var c EnergyCell
		err := r.Scan(&c.Day, &c.Org, &c.Lab, &c.Team, &c.Project, &c.User, &c.Model, &c.Pool, &c.MJ)
		return c, err
	})
	if err != nil {
		return nil, nil, err
	}
	rows, err = p.pool.Query(ctx, `
		SELECT to_char(ts_start AT TIME ZONE 'UTC', 'YYYY-MM-DD'), sum(idle_mj)::bigint
		FROM llmflow_energy_intervals WHERE ts_start >= $1 AND ts_start < $2 GROUP BY 1 ORDER BY 1`, from, to)
	if err != nil {
		return nil, nil, err
	}
	idle, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnergyIdle, error) {
		var i EnergyIdle
		err := r.Scan(&i.Day, &i.MJ)
		return i, err
	})
	return cells, idle, err
}

func (p *Postgres) LastEnergy(ctx context.Context, upstream string) (time.Time, error) {
	var t *time.Time
	err := p.pool.QueryRow(ctx, `SELECT max(ts_end) FROM llmflow_energy_intervals WHERE upstream = $1`, upstream).Scan(&t)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}

var (
	_ EnergyStore = (*Memory)(nil)
	_ EnergyStore = (*Postgres)(nil)
)
