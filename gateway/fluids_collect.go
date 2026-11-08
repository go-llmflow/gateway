package gateway

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
	"github.com/go-redfish/redfish"
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
	key    string   // the interval's "upstream" in the store: scheme://host of the exporter
	url    string   // the exporter's URL
	labels []string // the upstream labels of the records served on this node
	// pool is the pool the node's GPUs belong to (the models served on it):
	// its idle energy is allocated by that pool's shares. "" when the models
	// of the node belong to several pools (config refuses that when one of
	// them has shares of its own): the organisations' shares apply.
	pool  string
	mixed bool
	meter energy.Meter
	// The energy beyond the GPUs (nodeenergy.go): the BMC's counter when the
	// node has one, the fallback factor otherwise (1: none).
	bmc      *energy.RedfishScraper
	bmcMeter redfish.Meter
	factor   float64
	// The vLLM preemption counters of the node's upstreams (their base URLs),
	// as last read, and the preemptions seen since the last interval ended
	// (preemptBad: a counter could not be read, the count is then unknown).
	upstreams   []string
	preemptPrev map[string]float64
	preemptAcc  int64
	preemptBad  bool
	pending     []pendingInterval // measured, waiting for the accounting to settle
}

// pendingInterval is a measured interval with what was learnt beside the GPUs.
type pendingInterval struct {
	iv         energy.Interval
	overheadMJ int64
	overheadBy string // "redfish", "factor" or ""
	preempt    int64  // vLLM preemptions in the interval, -1 unknown
}

// energyNodes lists the configured exporters. secrets holds the BMC
// passwords, by redfish_password_file reference.
func energyNodes(cfg *config.Config, secrets Secrets) []*energyNode {
	var nodes []*energyNode
	by := map[string]*energyNode{}
	for _, g := range cfg.ModelGroups {
		for _, u := range g.Upstreams {
			if u.DcgmURL == "" {
				continue
			}
			n := by[u.DcgmURL]
			if n == nil {
				n = &energyNode{key: redactURL(u.DcgmURL), url: u.DcgmURL, pool: g.Pool, factor: cfg.Fluids.NodeOverheadFactor, preemptPrev: map[string]float64{}}
				n.meter.MaxGap = 3 * cfg.Fluids.Interval
				n.meter.MaxWatts = cfg.Fluids.MaxGPUWatts
				n.bmcMeter.MaxGap = 3 * cfg.Fluids.Interval
				by[u.DcgmURL] = n
				nodes = append(nodes, n)
			} else if n.pool != g.Pool {
				n.mixed, n.pool = true, ""
			}
			if l := redactURL(u.BaseURL); !slices.Contains(n.labels, l) {
				n.labels = append(n.labels, l)
			}
			if !slices.Contains(n.upstreams, u.BaseURL) {
				n.upstreams = append(n.upstreams, u.BaseURL)
			}
			if u.NodeOverheadFactor > 0 {
				n.factor = u.NodeOverheadFactor
			}
			if u.RedfishURL != "" && n.bmc == nil {
				n.bmc = &energy.RedfishScraper{URL: u.RedfishURL, User: u.RedfishUser, Password: secrets.RedfishPasswords[u.RedfishPasswordFile], CAFile: u.RedfishCAFile}
			}
		}
	}
	return nodes
}

// attributeInterval splits one measured interval: its active energy between
// the consumers by weighted work, the rest (idle, and active energy of an
// interval without work) stays whole. The parts sum to the measured energy.
//
// The node's energy beyond the GPUs (overheadMJ) is split like the GPUs': in
// the proportion of the GPUs' active and idle energy of the interval, then
// the active part by the same weights as the GPUs' active energy, the idle
// part as idle. The total is the GPUs' plus the overhead, exactly.
func attributeInterval(upstream, pool string, p pendingInterval, work []energy.Work, f *config.Fluids) (store.EnergyInterval, []energy.Attributed) {
	iv := p.iv
	active, idle := iv.SplitIdle(f.IdleWatts)
	if p.overheadMJ > 0 {
		oa := int64(0)
		if parts := energy.Apportion(p.overheadMJ, []float64{float64(active), float64(idle)}); parts != nil {
			oa = parts[0]
		}
		active += oa
		idle += p.overheadMJ - oa
	}
	att, left := energy.AttributeActive(active, work, f.Weights())
	return store.EnergyInterval{
		Upstream: upstream, Start: iv.Start, End: iv.End, TotalMJ: iv.MJ + p.overheadMJ, IdleMJ: idle + left,
		GPUs: len(iv.PerGPU), Resets: iv.Resets, Gap: iv.Gap, Pool: pool,
		OverheadMJ: p.overheadMJ, OverheadSource: p.overheadBy, Preemptions: p.preempt,
	}, att
}

// scrapePreemptions reads the vLLM preemption counter of every upstream of
// the node and adds what happened since the last read (a counter that went
// down is a restart: its new value is what happened since).
func (s *Server) scrapePreemptions(ctx context.Context, n *energyNode) {
	for _, base := range n.upstreams {
		v, ok := s.scrapeCounter(ctx, base, preemptionMetrics...)
		if !ok {
			n.preemptBad = true
			continue
		}
		prev, known := n.preemptPrev[base]
		n.preemptPrev[base] = v
		switch {
		case !known:
		case v >= prev:
			n.preemptAcc += int64(v - prev)
		default:
			n.preemptAcc += int64(v)
		}
	}
}

// takePreemptions returns the preemptions since the last interval ended (-1
// when a counter could not be read) and starts the next count.
func (n *energyNode) takePreemptions() int64 {
	v := n.preemptAcc
	if n.preemptBad || len(n.upstreams) == 0 {
		v = -1
	}
	n.preemptAcc, n.preemptBad = 0, false
	return v
}

// Sources of the node's overhead, stored with the interval and reported.
const (
	OverheadRedfish      = "redfish"       // the BMC's energy counter minus the GPUs'
	OverheadRedfishPower = "redfish_power" // the BMC's power, integrated (trapezoid), minus the GPUs'
	OverheadFactor       = "factor"        // node_overhead_factor
)

// overhead decides the node's energy beyond the GPUs for an interval: the
// BMC's energy (its counter's difference, else its power integrated between
// two samples no further apart than three rounds) minus the GPUs' when the BMC
// was read for exactly this interval, else the fallback factor, else none. A
// counter that went down (a BMC reboot) is never billed.
func (s *Server) overhead(n *energyNode, iv energy.Interval, bi redfish.Interval, bok bool) (int64, string) {
	switch {
	case bok && bi.Start.Equal(iv.Start) && bi.End.Equal(iv.End):
		if bi.Valid {
			nodeMJ := int64(math.Round(bi.KWh * energy.MJPerKWh))
			if nodeMJ >= iv.MJ {
				by := OverheadRedfish
				if bi.Source == redfish.SourcePower {
					by = OverheadRedfishPower
				}
				return nodeMJ - iv.MJ, by
			}
			s.log.Warn("fluids: the BMC reports less energy than the GPUs alone; no overhead counted for this interval", "node", n.key, "node_mj", nodeMJ, "gpu_mj", iv.MJ)
		} else {
			s.log.Warn("fluids: the BMC's energy for this interval is unknown; the fallback factor applies", "node", n.key, "reason", bi.Reason)
		}
	case bok:
		s.log.Warn("fluids: the BMC's interval is not the GPUs' (a read was missed); the fallback factor applies", "node", n.key)
	case n.bmc != nil:
		s.log.Warn("fluids: the BMC was not read for this interval; the fallback factor applies", "node", n.key)
	}
	if n.factor > 1 {
		return int64(math.Round(float64(iv.MJ) * (n.factor - 1))), OverheadFactor
	}
	return 0, ""
}

const maxPendingIntervals = 2000

// startFluids starts the scraping loop when a fluids block and at least one
// dcgm_url exist.
func (s *Server) startFluids(ctx context.Context) {
	if s.cfg.Fluids == nil {
		return
	}
	nodes := energyNodes(s.cfg, s.secrets)
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
		now := s.now()
		sc := energy.Scraper{URL: n.url, Client: s.client, Timeout: s.cfg.Health.Timeout}
		rd, err := sc.Scrape(ctx, now)
		if err != nil {
			s.log.Warn("fluids: scrape failed; its energy is counted in the next interval", "node", n.key, "err", redactErr(err))
			continue
		}
		// The BMC, at the same instant (an interval is the GPUs' and the node's
		// over the same time); a failed read leaves the fallback to this interval.
		var bi redfish.Interval
		var bok bool
		if n.bmc != nil {
			n.bmc.Timeout = s.cfg.Health.Timeout
			if brd, berr := n.bmc.Read(ctx); berr != nil {
				s.log.Warn("fluids: the BMC could not be read", "node", n.key, "err", redactErr(berr))
			} else {
				brd.At = now // the GPUs' instant: the two intervals are the same time
				bi, bok = n.bmcMeter.Observe(brd)
			}
		}
		s.scrapePreemptions(ctx, n)
		if iv, ok := n.meter.Observe(rd); ok {
			if iv.Glitches > 0 || iv.Capped > 0 {
				// Where an operator reads it: a counter that dipped and came back
				// (discarded), or an interval cut to the device's power.
				s.log.Warn("fluids: an exporter's counter was not trusted", "node", n.key,
					"glitches_discarded", iv.Glitches, "capped_at_max_gpu_watts", iv.Capped, "resets", iv.Resets)
			}
			if len(n.pending) >= maxPendingIntervals { // the store has been failing for hours
				s.log.Error("fluids: too many unstored intervals; dropping the oldest", "node", n.key)
				n.pending = n.pending[1:]
			}
			oh, by := s.overhead(n, iv, bi, bok)
			n.pending = append(n.pending, pendingInterval{iv: iv, overheadMJ: oh, overheadBy: by, preempt: n.takePreemptions()})
		}
		cutoff := s.now().Add(-f.Settle)
		for len(n.pending) > 0 && !n.pending[0].iv.End.After(cutoff) {
			iv := n.pending[0]
			work, err := es.WorkBetween(ctx, n.labels, iv.iv.Start, iv.iv.End)
			if err != nil {
				s.log.Error("fluids: reading the work of an interval failed; will retry", "node", n.key, "err", err)
				break
			}
			ei, att := attributeInterval(n.key, n.pool, iv, work, f)
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
