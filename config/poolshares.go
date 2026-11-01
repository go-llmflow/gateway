package config

import (
	"math"
	"sort"
)

// Per-pool shares (since the release that added them).
//
// A machine bought by several institutions is apportioned by what each paid
// for it, and that is not the apportionment of another machine:
//
//	pool "rtx6000" { capacity = 64  shares = { etabA = 0.5, etabC = 0.3, etabD = 0.2 } }
//
// A pool without shares keeps the organisations' shares (org { share }), so a
// configuration written before this release means what it meant. An
// organisation absent from a pool's shares holds 0 slots there.
//
// Slurm does the same thing with associations: a share (fairshare) is held by
// an association, and associations are per cluster and optionally per
// partition (sshare "-M cluster"; the Fair Tree page, slurm.schedmd.com/
// fair_tree.html, ranks the accounts of one association tree). The gateway's
// pool is the partition: its tree is the pool's own shares.

// Borrow values: who may use the capacity the shareholders leave idle.
const (
	// BorrowAll lends idle capacity to any organisation, at borrowing
	// priority (the behaviour before per-pool shares).
	BorrowAll = "all"
	// BorrowShareholders lends it only to the pool's contributors: an
	// organisation holding no share of the pool is refused (403).
	BorrowShareholders = "shareholders"
)

// PoolShares returns the shares of the pool's organisations: the pool's own
// when it has them, else the organisations'. The map is the caller's.
func (c *Config) PoolShares(pool string) map[string]float64 {
	out := map[string]float64{}
	for _, p := range c.Pools {
		if p.Name == pool && p.Shares != nil {
			for o, s := range p.Shares {
				out[o] = s
			}
			return out
		}
	}
	for _, o := range c.Orgs {
		out[o.Name] = o.Share
	}
	return out
}

// PoolShare is one organisation's share of one pool.
func (c *Config) PoolShare(pool, org string) float64 { return c.PoolShares(pool)[org] }

// PoolBorrow returns the pool's borrow policy.
func (c *Config) PoolBorrow(pool string) string {
	for _, p := range c.Pools {
		if p.Name == pool && p.Borrow != "" {
			return p.Borrow
		}
	}
	return BorrowAll
}

// PoolAllows reports whether org may use the pool at all: always, unless the
// pool lends only to its shareholders and org holds none of it.
func (c *Config) PoolAllows(pool, org string) bool {
	return c.PoolBorrow(pool) != BorrowShareholders || c.PoolShare(pool, org) > 0
}

// HasPoolShares reports whether the pool has an apportionment of its own.
func (c *Config) HasPoolShares(pool string) bool {
	for _, p := range c.Pools {
		if p.Name == pool {
			return p.Shares != nil
		}
	}
	return false
}

// PoolsWithShares lists the pools that have shares of their own, sorted.
func (c *Config) PoolsWithShares() []string {
	var out []string
	for _, p := range c.Pools {
		if p.Shares != nil {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out
}

// dcgmPools refuses a DCGM exporter shared by models of pools with different
// apportionments: the idle energy of a node is allocated by the shares of the
// pool the node belongs to, and a node serving two pools has no such pool.
func (v *validator) dcgmPools(raw *fileHCL, c *Config) {
	pool := map[string]string{} // dcgm_url → pool of its first model
	for i, g := range c.ModelGroups {
		if i >= len(raw.Models) {
			break
		}
		for j, u := range g.Upstreams {
			if u.DcgmURL == "" {
				continue
			}
			first, seen := pool[u.DcgmURL]
			if !seen {
				pool[u.DcgmURL] = g.Pool
				continue
			}
			if first != g.Pool && (c.HasPoolShares(first) || c.HasPoolShares(g.Pool)) {
				r := raw.Models[i].DefRange
				if j < len(raw.Models[i].Upstreams) {
					r = raw.Models[i].Upstreams[j].DcgmURLRange
				}
				v.errf(r, "dcgm_url shared by two pools", "the DCGM exporter %s serves models of pools %q and %q, one of which has shares of its own: the idle energy of a node is allocated by the shares of its pool, so a node belongs to one pool",
					redactDCGM(u.DcgmURL), first, g.Pool)
			}
		}
	}
}

func redactDCGM(raw string) string {
	if i := indexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// poolShares validates the shares and borrow of a pool. The checks are those
// of the organisations' shares (each in 0..1, a sum of at most 1), plus: every
// named organisation exists.
func (v *validator) poolShares(p poolHCL, pool *Pool, orgs map[string]bool) {
	pool.Borrow = BorrowAll
	if p.Borrow != nil {
		switch *p.Borrow {
		case BorrowAll, BorrowShareholders:
			pool.Borrow = *p.Borrow
		default:
			v.errf(p.BorrowRange, "Invalid borrow", "pool %q borrow = %q: want \"all\" or \"shareholders\"", p.Name, *p.Borrow)
		}
	}
	if p.Shares == nil {
		return
	}
	pool.Shares = map[string]float64{}
	names := make([]string, 0, len(p.Shares))
	for o := range p.Shares {
		names = append(names, o)
	}
	sort.Strings(names)
	sum := 0.0
	for _, o := range names {
		s := p.Shares[o]
		if !orgs[o] {
			v.errf(p.SharesRange, "Unknown organisation", "pool %q shares name %q, which is not declared in an org block", p.Name, o)
			continue
		}
		if !(s >= 0 && s <= 1) || math.IsNaN(s) {
			v.errf(p.SharesRange, "Invalid share", "pool %q share of %q = %v: want 0..1", p.Name, o, s)
			continue
		}
		pool.Shares[o] = s
		sum += s
	}
	if sum > 1+1e-9 {
		v.errf(p.SharesRange, "Shares exceed the pool", "pool %q shares sum to %v: want at most 1", p.Name, sum)
	}
	if len(p.Shares) == 0 {
		v.errf(p.SharesRange, "Empty shares", "pool %q: shares = {} holds no organisation; omit it to use the organisations' shares", p.Name)
	}
}
