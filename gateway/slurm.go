package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/gateway/config"
)

// Slurm discovery (#75): the vLLM instances of a model run as long Slurm
// jobs of a service account; the gateway follows the RUNNING jobs that
// follow the convention, through slurmrestd, and never starts or stops
// one (scrontab or the operator does).
//
// References opened 2026-10-26 (see config/slurm.go): Slurm master
// src/slurmrestd/plugins/openapi/slurmctld/api.c — GET
// /slurm/{data_parser}/jobs/ answers {jobs, last_update, errors, warnings};
// src/plugins/data_parser/v0.0.44/parsers.c PARSER_ARRAY(JOB_INFO) —
// job_id (uint32), name, comment, account, partition, user_name,
// batch_host, job_state (a flag array: a base state such as "RUNNING" and
// flags such as "COMPLETING"); src/slurmrestd/plugins/auth/jwt/jwt.c —
// X-SLURM-USER-TOKEN and X-SLURM-USER-NAME. vLLM v0.32.0
// entrypoints/serve/instrumentator/health.py (GET /health: 200, 503 when
// the engine is dead) and entrypoints/openai/models/api_router.py (GET
// /v1/models: {data: [{id}]}, behind --api-key).

const (
	// slurmConvention prefixes a job's name or comment:
	// llmflow:<model>:<port>[:<capacity>].
	slurmConvention = "llmflow:"
	// maxSlurmBody bounds slurmrestd's answer; maxSlurmJobs the jobs read.
	maxSlurmBody = 32 << 20
	maxSlurmJobs = 100_000
	// maxModelsBody bounds an instance's /v1/models answer.
	maxModelsBody = 1 << 20
	// slurmHealthFailures: consecutive failed probes before a member drains.
	slurmHealthFailures = 3
)

// hostRe accepts a host name as batch_host carries it: letters, digits,
// dots and hyphens; nothing that could carry a port, a path or
// credentials into the URL built from it.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,252})$`)

type slurmDiscovery struct {
	s     *Server
	g     *group
	cfg   config.SlurmJobs
	key   string // the instances' vLLM --api-key
	token string // slurmrestd's JWT
	// static is the group's configured upstreams, always kept.
	static []*upstream
	client *http.Client

	mu      sync.Mutex
	members map[string]*slurmMember // by base URL
	lastOK  time.Time
	stale   bool
	ignored map[string]bool // foreign jobs already reported, by job id and reason
	cap     atomic.Int64    // see publish
}

type slurmMember struct {
	u          *upstream
	jobID      uint32
	capacity   int
	fails      int
	drainSince time.Time
}

// slurmJob is the part of a JOB_INFO the gateway reads.
type slurmJob struct {
	JobID     uint32          `json:"job_id"`
	Name      string          `json:"name"`
	Comment   string          `json:"comment"`
	Account   string          `json:"account"`
	Partition string          `json:"partition"`
	UserName  string          `json:"user_name"`
	BatchHost string          `json:"batch_host"`
	JobState  json.RawMessage `json:"job_state"`
}

// states reads job_state: a flag array since data_parser v0.0.40, a plain
// string before.
func (j slurmJob) states() []string {
	var list []string
	if err := json.Unmarshal(j.JobState, &list); err == nil {
		return list
	}
	var one string
	if err := json.Unmarshal(j.JobState, &one); err == nil {
		return strings.Fields(strings.ReplaceAll(one, ",", " "))
	}
	return nil
}

// running: RUNNING, and none of the flags of a job on its way in or out.
func (j slurmJob) running() bool {
	st := j.states()
	if !slices.Contains(st, "RUNNING") {
		return false
	}
	for _, f := range []string{"COMPLETING", "CONFIGURING", "STOPPED", "SIGNALING", "RESIZING", "REQUEUED"} {
		if slices.Contains(st, f) {
			return false
		}
	}
	return true
}

func newSlurmDiscovery(s *Server, g *group, keys map[string]string) (*slurmDiscovery, error) {
	c := g.cfg.Slurm
	d := &slurmDiscovery{s: s, g: g, cfg: *c, key: keys[c.KeyFile], token: keys[c.TokenFile],
		static: g.members(), members: map[string]*slurmMember{}, ignored: map[string]bool{}}
	if d.key == "" {
		return nil, fmt.Errorf("gateway: model %q: no key loaded for its slurm instances (key_file)", g.cfg.Name)
	}
	if d.token == "" {
		return nil, fmt.Errorf("gateway: model %q: no slurmrestd token loaded (token_file)", g.cfg.Name)
	}
	if err := config.ValidateUpstream(c.URL); err != nil {
		return nil, err
	}
	d.client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil, // never through an environment proxy
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConnsPerHost:   2,
		},
		// The URL is the configuration's: a redirect would let slurmrestd
		// (or whoever answers in its place) send the JWT elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return d, nil
}

// loop polls until ctx ends.
func (d *slurmDiscovery) loop(ctx context.Context) {
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		d.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// candidate is a job that qualifies as an instance of the model.
type candidate struct {
	base     string
	jobID    uint32
	capacity int
}

// poll reads the jobs once and reconciles the members.
func (d *slurmDiscovery) poll(ctx context.Context) {
	jobs, err := d.fetch(ctx)
	now := d.s.now()
	model := d.g.cfg.Name
	d.mu.Lock()
	if err != nil {
		d.s.met.slurmPolls.add(1, model, "error")
		age := now.Sub(d.lastOK)
		if !d.stale && (d.lastOK.IsZero() || age > d.cfg.StaleAfter) {
			d.stale = true
			d.s.log.Error("slurm: slurmrestd unreadable past stale_after: no instance is admitted until it answers; healthy members are kept",
				"model", model, "err", err, "last_ok", d.lastOK)
		} else {
			d.s.log.Warn("slurm: slurmrestd unreadable; the last known members are kept", "model", model, "err", err)
		}
		d.mu.Unlock()
		d.maintain(ctx, now, nil)
		return
	}
	d.s.met.slurmPolls.add(1, model, "ok")
	if d.stale {
		d.s.log.Info("slurm: slurmrestd answers again", "model", model)
	}
	d.lastOK, d.stale = now, false
	want := map[string]candidate{}
	for _, j := range jobs {
		if c, ok := d.qualify(j); ok {
			if prev, dup := want[c.base]; !dup || c.jobID > prev.jobID {
				want[c.base] = c
			}
		}
	}
	d.mu.Unlock()
	d.maintain(ctx, now, want)
}

// qualify applies the convention and the trust rules to one job. Called
// with d.mu held.
func (d *slurmDiscovery) qualify(j slurmJob) (candidate, bool) {
	tag := ""
	for _, s := range []string{j.Name, j.Comment} {
		if strings.HasPrefix(s, slurmConvention+d.g.cfg.Name+":") {
			tag = strings.TrimPrefix(s, slurmConvention+d.g.cfg.Name+":")
			break
		}
	}
	if tag == "" || !j.running() {
		return candidate{}, false
	}
	reject := func(reason string) (candidate, bool) {
		k := strconv.FormatUint(uint64(j.JobID), 10) + "/" + reason
		if len(d.ignored) > 10_000 {
			clear(d.ignored) // bounded: a flood of fake jobs is reported again, not remembered forever
		}
		if !d.ignored[k] {
			d.ignored[k] = true
			d.s.met.slurmRejected.add(1, d.g.cfg.Name, reason)
			d.s.log.Warn("slurm: a job claims to serve the model but is not trusted; ignored",
				"model", d.g.cfg.Name, "job_id", j.JobID, "reason", reason, "account", j.Account, "partition", j.Partition, "user", j.UserName)
		}
		return candidate{}, false
	}
	switch {
	case !slices.Contains(d.cfg.Accounts, j.Account):
		return reject("account")
	case !slices.Contains(d.cfg.Partitions, j.Partition):
		return reject("partition")
	case len(d.cfg.Users) > 0 && !slices.Contains(d.cfg.Users, j.UserName):
		return reject("user")
	case !hostRe.MatchString(j.BatchHost):
		return reject("host")
	}
	parts := strings.Split(tag, ":")
	if len(parts) > 2 {
		return reject("convention")
	}
	port, err := strconv.Atoi(parts[0])
	if err != nil || port < 1 || port > 65535 || parts[0] != strconv.Itoa(port) {
		return reject("convention")
	}
	capacity := d.cfg.InstanceCapacity
	if len(parts) == 2 {
		capacity, err = strconv.Atoi(parts[1])
		if err != nil || capacity < 1 || capacity > d.cfg.MaxInstanceCapacity {
			return reject("capacity")
		}
	}
	base := d.cfg.Scheme + "://" + net.JoinHostPort(j.BatchHost, strconv.Itoa(port))
	if config.ValidateUpstream(base) != nil {
		return reject("host")
	}
	return candidate{base: base, jobID: j.JobID, capacity: capacity}, true
}

// fetch reads the jobs from slurmrestd.
func (d *slurmDiscovery) fetch(ctx context.Context) ([]slurmJob, error) {
	ctx, cancel := context.WithTimeout(ctx, d.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.cfg.URL+"/slurm/"+d.cfg.APIVersion+"/jobs/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-SLURM-USER-TOKEN", d.token)
	if d.cfg.UserName != "" {
		req.Header.Set("X-SLURM-USER-NAME", d.cfg.UserName)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body is not echoed: it may quote the request.
		return nil, fmt.Errorf("slurmrestd answered %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSlurmBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSlurmBody {
		return nil, errors.New("slurmrestd answer too large")
	}
	var out struct {
		Jobs   []slurmJob `json:"jobs"`
		Errors []struct {
			Error       string `json:"error"`
			Description string `json:"description"`
		} `json:"errors"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("slurmrestd answer: %w", err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("slurmrestd error: %.200s %.200s", out.Errors[0].Error, out.Errors[0].Description)
	}
	if out.Jobs == nil {
		return nil, errors.New("slurmrestd answer has no jobs list")
	}
	if len(out.Jobs) > maxSlurmJobs {
		return nil, fmt.Errorf("slurmrestd listed %d jobs, more than %d", len(out.Jobs), maxSlurmJobs)
	}
	return out.Jobs, nil
}

// verify checks an instance before it takes traffic: /health answers 200
// and /v1/models lists the model the group sends.
func (d *slurmDiscovery) verify(ctx context.Context, base string) bool {
	u := newUpstream(base, d.key)
	if !d.s.probe(ctx, u) {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, d.s.cfg.Health.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.joinURL("/v1/models"), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+d.key)
	resp, err := d.s.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelsBody)).Decode(&list); err != nil {
		return false
	}
	for _, m := range list.Data {
		if m.ID == d.g.cfg.UpstreamModel {
			return true
		}
	}
	return false
}

// maintain reconciles the members with want (nil: slurmrestd unread, the
// set is kept): new instances verified and added, members whose job is
// gone or that keep failing health drained, idle drained members removed.
func (d *slurmDiscovery) maintain(ctx context.Context, now time.Time, want map[string]candidate) {
	// Probes and verifications happen outside the lock.
	d.mu.Lock()
	var probe []*slurmMember
	var admit []candidate
	for _, m := range d.members {
		if m.drainSince.IsZero() {
			probe = append(probe, m)
		}
	}
	if want != nil {
		for base, c := range want {
			if _, ok := d.members[base]; !ok {
				admit = append(admit, c)
			}
		}
	}
	d.mu.Unlock()
	sort.Slice(admit, func(i, j int) bool { return admit[i].base < admit[j].base })
	healthy := map[*slurmMember]bool{}
	for _, m := range probe {
		healthy[m] = d.s.probe(ctx, m.u)
	}
	var ok []candidate
	for _, c := range admit {
		if d.verify(ctx, c.base) {
			ok = append(ok, c)
		} else {
			d.s.log.Info("slurm: instance not ready yet (health or /v1/models)", "model", d.g.cfg.Name, "upstream", redactURL(c.base), "job_id", c.jobID)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	changed := false
	for _, m := range probe {
		if d.members[m.u.base] != m || !m.drainSince.IsZero() {
			continue
		}
		if healthy[m] {
			m.fails = 0
		} else {
			m.fails++
		}
		reason := ""
		switch {
		case want != nil && want[m.u.base].jobID != m.jobID:
			reason = "job ended" // or another job took its place: re-verified once reaped
		case m.fails >= slurmHealthFailures:
			reason = "health"
		}
		if reason != "" {
			m.drainSince = now
			m.u.draining.Store(true)
			changed = true
			d.event("drained", m, reason)
		}
	}
	for _, c := range ok {
		if _, exists := d.members[c.base]; exists {
			continue
		}
		u := newUpstream(c.base, d.key)
		m := &slurmMember{u: u, jobID: c.jobID, capacity: c.capacity}
		d.members[c.base] = m
		changed = true
		d.event("added", m, "")
	}
	for base, m := range d.members {
		if m.drainSince.IsZero() {
			continue
		}
		if m.u.inflight.Load() == 0 || now.Sub(m.drainSince) > d.s.cfg.Limits.MaxRequestDuration {
			delete(d.members, base)
			changed = true
			d.event("removed", m, "")
		}
	}
	if changed {
		d.publish()
	}
}

// event logs and audits a membership change. Called with d.mu held.
func (d *slurmDiscovery) event(what string, m *slurmMember, reason string) {
	args := []any{"model", d.g.cfg.Name, "upstream", redactURL(m.u.base), "job_id", m.jobID, "capacity", m.capacity}
	if reason != "" {
		args = append(args, "reason", reason)
	}
	d.s.audit("slurm.member_"+what, args...)
}

// publish installs the new upstream set (static + members, draining ones
// included so their requests finish) and recomputes the pool's capacity.
// Called with d.mu held.
func (d *slurmDiscovery) publish() {
	set := append([]*upstream(nil), d.static...)
	bases := make([]string, 0, len(d.members))
	for b := range d.members {
		bases = append(bases, b)
	}
	sort.Strings(bases)
	for _, b := range bases {
		set = append(set, d.members[b].u)
	}
	d.g.setMembers(set)
	// The members' contribution to the pool's C: draining members take no
	// new request and count for nothing. An atomic, so that recomputing a
	// pool never takes another discovery's lock.
	n := 0
	for _, m := range d.members {
		if m.drainSince.IsZero() {
			n += m.capacity
		}
	}
	d.cap.Store(int64(n))
	d.s.recomputeCapacity(d.g.cfg.Pool)
}

func (d *slurmDiscovery) capacity() int { return int(d.cap.Load()) }

// lastSuccess is the time of the last successful poll (zero: none yet).
func (d *slurmDiscovery) lastSuccess() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastOK
}

// recomputeCapacity sets a pool's C to its configured capacity plus the
// capacity of its discovered instances, re-apportions the reservations
// (Hamilton, fairshare.Pool.SetCapacity) and audits a change.
func (s *Server) recomputeCapacity(pool string) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	p, ok := s.pools[pool].(elasticPool)
	if !ok {
		// A pool decided by its owner among replicas (ha) has a fixed C:
		// the configuration refuses slurm discovery with an ha block.
		return
	}
	c := 0
	for _, cp := range s.cfg.Pools {
		if cp.Name == pool {
			c = cp.Capacity
		}
	}
	for _, gn := range s.gorder {
		g := s.groups[gn]
		if g.disc != nil && g.cfg.Pool == pool {
			c += g.disc.capacity()
		}
	}
	if old := p.setCapacity(c); old != c {
		reserved, _ := json.Marshal(p.reserved())
		s.audit("pool.capacity", "pool", pool, "before", old, "after", c, "reserved", string(reserved))
	}
}

// elasticPool is a pool whose C can change (a pool of this replica).
type elasticPool interface {
	setCapacity(int) int
	capacity() int
	reserved() map[string]int
}

func (l localPool) setCapacity(c int) int    { return l.p.SetCapacity(c) }
func (l localPool) capacity() int            { return l.p.Capacity() }
func (l localPool) reserved() map[string]int { return l.p.Reserved() }

// poolCapacity is a pool's current C: its elastic value on this replica,
// the configured one otherwise.
func (s *Server) poolCapacity(name string) int {
	if p, ok := s.pools[name].(elasticPool); ok {
		return p.capacity()
	}
	for _, cp := range s.cfg.Pools {
		if cp.Name == name {
			return cp.Capacity
		}
	}
	return 0
}
