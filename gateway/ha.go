package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-llmflow/gateway/fairshare"
	"github.com/go-llmflow/gateway/ha"
	"github.com/go-llmflow/gateway/store"
)

// admitter decides the fair-share standing of a request in one pool: the
// pool itself in one replica, or the pool's owner among several (ha).
type admitter interface {
	acquire(org string, class fairshare.Class, est int64) (*fairshare.Grant, error)
	tryAcquire(org string, class fairshare.Class, est int64) (*fairshare.Grant, bool)
	// snapshot is the pool's state as this replica knows it (nil on a
	// replica that does not own it).
	snapshot() []fairshare.TeamStat
}

type localPool struct{ p *fairshare.Pool }

func (l localPool) acquire(org string, class fairshare.Class, est int64) (*fairshare.Grant, error) {
	return l.p.Acquire(org, class, est), nil
}

func (l localPool) tryAcquire(org string, class fairshare.Class, est int64) (*fairshare.Grant, bool) {
	return l.p.TryAcquire(org, class, est)
}

func (l localPool) snapshot() []fairshare.TeamStat { return l.p.Snapshot() }

type haPool struct {
	n    *ha.Node
	name string
}

func (h haPool) acquire(org string, class fairshare.Class, est int64) (*fairshare.Grant, error) {
	g, _, err := h.n.Acquire(h.name, org, class, est, false)
	return g, err
}

func (h haPool) tryAcquire(org string, class fairshare.Class, est int64) (*fairshare.Grant, bool) {
	g, ok, _ := h.n.Acquire(h.name, org, class, est, true)
	return g, ok
}

func (h haPool) snapshot() []fairshare.TeamStat { return h.n.Snapshot(h.name) }

// errNoOwner answers a request that found no reachable owner, with
// on_owner_unreachable = "reject".
var errNoOwner = errors.New("the replica that decides this pool's fair share cannot be reached; retry shortly")

// buildHA makes the pools shared: every pool is decided by its owner
// among the replicas (package ha).
func (s *Server) buildHA(shares map[string]float64) error {
	h := s.cfg.HA
	hs, ok := s.st.(store.HAStore)
	if !ok {
		return errors.New("gateway: the ha block needs the postgres store")
	}
	if len(s.secrets.HASecret) < ha.MinSecret {
		return fmt.Errorf("gateway: the ha secret must be at least %d bytes", ha.MinSecret)
	}
	id := h.ReplicaID
	if id == "" {
		id, _ = os.Hostname()
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	inst := id + "-" + hex.EncodeToString(b[:])
	specs := make([]ha.PoolSpec, 0, len(s.cfg.Pools))
	for _, p := range s.cfg.Pools {
		specs = append(specs, ha.PoolSpec{Name: p.Name, Capacity: p.Capacity, Shares: shares, HalfLife: s.cfg.FairShare.HalfLife})
	}
	n, err := ha.New(ha.Config{
		Instance: inst, Advertise: h.Advertise, Secret: []byte(s.secrets.HASecret), PeerNetworks: h.PeerNetworks,
		LeaseTTL: h.LeaseTTL, Heartbeat: h.Heartbeat, ForwardTimeout: h.ForwardTimeout, UsagePersist: h.UsagePersist,
		MaxRequest: s.cfg.Limits.MaxRequestDuration, RejectUnreachable: h.OnOwnerUnreachable == "reject",
		Pools: specs, Now: s.now, Log: s.log, Membership: s.poolMembership,
	}, hs)
	if err != nil {
		return err
	}
	s.ha = n
	if s.shared, ok = s.st.(store.HAStateStore); !ok {
		return errors.New("gateway: the ha block needs the postgres store")
	}
	for _, p := range s.cfg.Pools {
		s.pools[p.Name] = haPool{n: n, name: p.Name}
	}
	return nil
}

// poolMembership tells package ha whether this replica knows a pool's
// members (#75): every replica runs its own Slurm discovery, with its own
// health checks, for its own routing; only the owner's view sets C. A
// replica takes a pool only once each of its Slurm-discovered models has
// been read, and an owner whose slurmrestd stops answering (stale) yields
// the pool to a replica that still reads it.
func (s *Server) poolMembership(pool string) (ready, fresh bool) {
	ready, fresh = true, true
	for _, gn := range s.gorder {
		g := s.groups[gn]
		if g.disc == nil || g.cfg.Pool != pool {
			continue
		}
		r, f := g.disc.known()
		ready, fresh = ready && r, fresh && f
	}
	return ready, fresh
}

// startHA opens the internal listener and claims the leases.
func (s *Server) startHA(ctx context.Context) error {
	if s.ha == nil {
		return nil
	}
	ln := s.haListener
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", s.cfg.HA.Listen); err != nil {
			return fmt.Errorf("gateway: ha listen: %w", err)
		}
	}
	s.haServer = &http.Server{
		Handler: s.ha.Handler(), ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10,
	}
	go func() { _ = s.haServer.Serve(ln) }()
	s.ha.Start(ctx)
	return nil
}

func (s *Server) closeHA(ctx context.Context) {
	if s.ha == nil {
		return
	}
	s.ha.Close(ctx)
	if s.haServer != nil {
		_ = s.haServer.Close()
	}
}

// WithHAListener makes the internal API listen on ln (tests: port 0).
func WithHAListener(ln net.Listener) Option { return func(s *Server) { s.haListener = ln } }
