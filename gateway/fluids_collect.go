package gateway

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

// Fluids collection (#97): the gateway scrapes the NVIDIA DCGM exporter of
// each node named by an upstream's dcgm_url, turns the energy counter into
// per-interval deltas (package energy), attributes each interval's active
// energy to the requests that finished in it, and stores the result.
//
// A node is a dcgm_url: several vLLM upstreams on one node share its GPUs,
// so their requests share its energy. Scraping runs on every replica but an
// interval is stored once: a second replica's overlapping interval is
// refused by the store (first wins). Energy used while the gateway is down
// is not counted: the first scrape after a start only sets the baseline.
// Slurm-discovered members have no dcgm_url yet and are not metered.

// energyNode is one DCGM exporter.
type energyNode struct {
	key     string   // the interval's "upstream" in the store: scheme://host of the exporter
	url     string   // the exporter's URL
	labels  []string // the upstream labels of the records served on this node
	meter   energy.Meter
	pending []energy.Interval // measured, waiting for the accounting to settle
}

// energyNodes lists the configured exporters.
func energyNodes(cfg *config.Config) []*energyNode {
	var nodes []*energyNode
	by := map[string]*energyNode{}
	for _, g := range cfg.ModelGroups {
		for _, u := range g.Upstreams {
			if u.DcgmURL == "" {
				continue
			}
			n := by[u.DcgmURL]
			if n == nil {
				n = &energyNode{key: redactURL(u.DcgmURL), url: u.DcgmURL}
				n.meter.MaxGap = 3 * cfg.Fluids.Interval
				by[u.DcgmURL] = n
				nodes = append(nodes, n)
			}
			if l := redactURL(u.BaseURL); !slices.Contains(n.labels, l) {
				n.labels = append(n.labels, l)
			}
		}
	}
	return nodes
}

// attributeInterval splits one measured interval: its active energy between
// the consumers by weighted work, the rest (idle, and active energy of an
// interval without work) stays whole. The parts sum to the measured energy.
func attributeInterval(upstream string, iv energy.Interval, work []energy.Work, f *config.Fluids) (store.EnergyInterval, []energy.Attributed) {
	active, idle := iv.SplitIdle(f.IdleWatts)
	att, left := energy.AttributeActive(active, work, f.Weights())
	return store.EnergyInterval{
		Upstream: upstream, Start: iv.Start, End: iv.End, TotalMJ: iv.MJ, IdleMJ: idle + left,
		GPUs: len(iv.PerGPU), Resets: iv.Resets, Gap: iv.Gap,
	}, att
}

const maxPendingIntervals = 2000

// startFluids starts the scraping loop when a fluids block and at least one
// dcgm_url exist.
func (s *Server) startFluids(ctx context.Context) {
	if s.cfg.Fluids == nil {
		return
	}
	nodes := energyNodes(s.cfg)
	if len(nodes) == 0 {
		return
	}
	es, ok := s.st.(store.EnergyStore)
	if !ok {
		s.log.Error("fluids: the store cannot keep energy; no DCGM exporter is scraped")
		return
	}
	s.bg.Add(1)
	go func() { defer s.bg.Done(); s.fluidsLoop(ctx, es, nodes) }()
}

// fluidsLoop scrapes until ctx ends.
func (s *Server) fluidsLoop(ctx context.Context, es store.EnergyStore, nodes []*energyNode) {
	f := s.cfg.Fluids
	t := time.NewTicker(f.Interval)
	defer t.Stop()
	for {
		s.fluidsRound(ctx, es, nodes)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// fluidsRound scrapes every node once and stores the intervals that settled.
func (s *Server) fluidsRound(ctx context.Context, es store.EnergyStore, nodes []*energyNode) {
	f := s.cfg.Fluids
	for _, n := range nodes {
		sc := energy.Scraper{URL: n.url, Client: s.client, Timeout: s.cfg.Health.Timeout}
		rd, err := sc.Scrape(ctx, s.now())
		if err != nil {
			s.log.Warn("fluids: scrape failed; its energy is counted in the next interval", "node", n.key, "err", redactErr(err))
			continue
		}
		if iv, ok := n.meter.Observe(rd); ok {
			if len(n.pending) >= maxPendingIntervals { // the store has been failing for hours
				s.log.Error("fluids: too many unstored intervals; dropping the oldest", "node", n.key)
				n.pending = n.pending[1:]
			}
			n.pending = append(n.pending, iv)
		}
		cutoff := s.now().Add(-f.Settle)
		for len(n.pending) > 0 && !n.pending[0].End.After(cutoff) {
			iv := n.pending[0]
			work, err := es.WorkBetween(ctx, n.labels, iv.Start, iv.End)
			if err != nil {
				s.log.Error("fluids: reading the work of an interval failed; will retry", "node", n.key, "err", err)
				break
			}
			ei, att := attributeInterval(n.key, iv, work, f)
			switch err := es.SaveEnergy(ctx, ei, att); {
			case err == nil, errors.Is(err, store.ErrConflict): // stored by another replica
			default:
				s.log.Error("fluids: storing an interval failed; will retry", "node", n.key, "err", err)
				return
			}
			n.pending = n.pending[1:]
		}
	}
}

// redactErr keeps an error's text but not a URL's credentials or query: the
// configured URLs carry none (config refuses them), this is a second guard
// for what an exporter or a proxy echoes.
func redactErr(err error) string {
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
