// Package ha lets several gateway replicas share pools without losing the
// fair-share guarantees, with PostgreSQL as the only shared component.
//
// Each pool's fair share is decided by exactly one replica at a time, its
// owner, which holds the pool's lease (store.HAStore). The lease's epoch
// is a fencing token (M. Kleppmann, "How to do distributed locking",
// 2016): it grows at every change of owner, the owner's writes to the
// database are checked against it under a row lock, and replicas refuse
// an answer from an epoch older than one they have seen. The other
// replicas forward the admission decision (acquire, release) to the owner
// over an authenticated internal channel (wire.go) and send the request
// to vLLM themselves.
//
// Expiry is measured with PostgreSQL's clock; the owner decides only
// while the monotonic time at which its last successful renewal STARTED,
// plus nine tenths of the lease's duration, is in the future. PostgreSQL
// cannot give the lease to anyone else before that renewal's expiry, so
// an owner cut from PostgreSQL stops deciding before another can start:
// no two owners decide at the same time, whatever the replicas' clocks.
//
// When the owner changes, the new one starts from the decayed usage the
// previous owner persisted (so the ranking survives), from the grants the
// surviving replicas report (each replica reports its live grants every
// heartbeat), and from a safety margin: the in-flight counts the previous
// owner persisted, as phantoms that expire after max_request_duration and
// shrink as reports account for them. A request that finds no reachable
// owner is admitted at the lowest borrowing priority (vLLM never lets a
// waiting request preempt a running one, and preempts the largest
// priority first, so it cannot take anyone's reservation), or refused.
package ha

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/gateway/fairshare"
	"github.com/go-llmflow/gateway/store"
)

// PoolSpec is one pool's fair-share configuration.
type PoolSpec struct {
	Name     string
	Capacity int
	Shares   map[string]float64
	// OwnShares: Shares are this pool's own apportionment (pool { shares }),
	// not the organisations'. Only a replica that knows per-pool shares can
	// decide such a pool, so the owner's published reservations carry a marker
	// (see shareMarkers) and a replica refuses an owner that does not.
	OwnShares bool
	HalfLife  time.Duration
}

// The reservations an owner publishes with its lease are a JSON object
// organisation -> integer. Two extra integer entries, whose names cannot be an
// organisation's (config.ValidName: [A-Za-z0-9_.-]), say where the shares come
// from: "@own" is 1 when the pool has shares of its own, and "@fp" is a
// fingerprint of the shares the reservations were computed from. A replica
// from before per-pool shares publishes neither: a replica whose pool has its
// own shares then knows the owner decides with the organisations' shares, and
// refuses to follow it (ErrSharesMismatch) instead of serving the pool with
// the wrong apportionment. An older replica reading the object sees two more
// "organisations" it never asks about.
const (
	markerOwn = "@own"
	markerFP  = "@fp"
)

// ErrSharesMismatch: the pool has shares of its own here, and its owner
// published reservations that are not computed from the same shares (an older
// replica, or a replica with another configuration).
var ErrSharesMismatch = errors.New("ha: this pool's owner does not use the pool's own shares (an older replica, or another configuration): update every replica to the same version and configuration")

// SharesFingerprint is a stable 52-bit fingerprint of an apportionment.
func SharesFingerprint(shares map[string]float64) int64 {
	names := make([]string, 0, len(shares))
	for k := range shares {
		names = append(names, k)
	}
	sort.Strings(names)
	h := uint64(14695981039346656037)
	mix := func(b []byte) {
		for _, c := range b {
			h ^= uint64(c)
			h *= 1099511628211
		}
	}
	for _, k := range names {
		mix([]byte(k))
		mix([]byte{0})
		mix([]byte(strconv.FormatFloat(shares[k], 'g', -1, 64)))
		mix([]byte{1})
	}
	return int64(h >> 12)
}

// ownerSharesOK reports whether the reservations an owner published (JSON)
// were computed from the shares fp describes.
func ownerSharesOK(published string, fp int64) bool {
	var m map[string]int64
	if json.Unmarshal([]byte(published), &m) != nil {
		return false
	}
	return m[markerOwn] == 1 && m[markerFP] == fp
}

// Config configures a Node.
type Config struct {
	// Instance names this process uniquely among the replicas (the
	// gateway adds a random suffix to the configured replica id, so a
	// restarted replica is a new instance).
	Instance string
	// Advertise is the address (IP:port) other replicas reach this one at.
	Advertise string
	// Secret is the shared secret (at least MinSecret bytes).
	Secret       []byte
	PeerNetworks []netip.Prefix
	// LeaseTTL is the lease duration; it is renewed every third.
	LeaseTTL time.Duration
	// Heartbeat is how often replicas report their grants to owners.
	Heartbeat time.Duration
	// ForwardTimeout bounds one call to a peer.
	ForwardTimeout time.Duration
	// UsagePersist is how often an owner persists its usage.
	UsagePersist time.Duration
	// MaxRequest is max_request_duration: how long a request of a dead
	// replica may still run.
	MaxRequest time.Duration
	// RejectUnreachable: without a reachable owner, refuse (ErrNoOwner)
	// instead of admitting at the lowest priority.
	RejectUnreachable bool
	Pools             []PoolSpec
	// Now is the wall clock (signatures, usage decay); time.Now if nil.
	// Lease validity never uses it.
	Now func() time.Time
	Log *slog.Logger
	// OnDecide, if set, is called for every decision this node makes as
	// an owner, with the pool's lock held (tests).
	OnDecide func(pool string, epoch int64, instance string)
	// Membership says whether this replica knows the pool's members
	// (ready: its Slurm discovery has read them once; fresh: and still
	// does, #75). A replica takes an expired lease only when ready, a
	// yielded one only when fresh, and an owner that is no longer fresh
	// yields. Nil: always ready and fresh (a pool of fixed upstreams).
	Membership func(pool string) (ready, fresh bool)
	// ClientTLS, when set, makes this node call its peers over TLS
	// (MutualTLS); their listener must then serve the matching server
	// configuration.
	ClientTLS *tls.Config
}

// ErrNoOwner: no owner could be reached and the configuration refuses
// requests in that case.
var ErrNoOwner = errors.New("ha: the pool's owner cannot be reached")

// Node is one replica's part.
type Node struct {
	cfg    Config
	st     store.HAStore
	client *http.Client
	rpcKey []byte
	replay *replayGuard

	seqMu sync.Mutex
	seqs  map[string]*atomic.Uint64

	pools map[string]*poolState
	order []string

	degraded atomic.Int64
	kick     chan struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	// sends are the releases on their way to an owner; life guards
	// stopped, so none starts once stop has begun waiting for them.
	life    sync.Mutex
	stopped bool
	sends   sync.WaitGroup
}

// stop ends the loops and waits for the releases in flight.
func (n *Node) stop() {
	if n.cancel != nil {
		n.cancel()
	}
	n.wg.Wait()
	n.life.Lock()
	n.stopped = true
	n.life.Unlock()
	n.sends.Wait()
}

type localGrant struct {
	id, org   string
	class     fairshare.Class
	tokens    int64
	priority  int
	borrowing bool
	// epoch that decided it (0: admitted without an owner).
	epoch int64
}

type poolState struct {
	spec PoolSpec

	mu sync.Mutex
	// as owner
	owner      bool
	epoch      int64
	validUntil time.Time // monotonic
	// notBefore: after taking a yielded lease, the previous owner may
	// still decide until its validity ends; this one waits a lease.
	notBefore time.Time
	eng       *engine
	lastSave  time.Time
	// capacity is C as this replica's own discovery sees it: the owner
	// decides with it and publishes it with the lease.
	capacity int
	// published C and reservations (JSON) of the current owner, from the
	// lease (-1: none).
	pubCapacity int
	pubReserved string
	// mismatch: the live owner's published reservations were not computed
	// from this replica's own shares for the pool (ErrSharesMismatch).
	mismatch bool
	// the lease as last read
	leaseOwner, leaseAddr string
	leaseEpoch            int64
	leaseLive             bool
	// known is the highest epoch seen: an answer from below is stale.
	known int64
	// this replica's live grants, and releases not delivered yet
	local   map[string]*localGrant
	pending []wireRelease
}

// maxPending bounds the releases kept for a later delivery.
const maxPending = 1 << 16

// New returns a node; Start begins its loops.
func New(cfg Config, st store.HAStore) (*Node, error) {
	if len(cfg.Secret) < MinSecret {
		return nil, errors.New("ha: the shared secret must be at least 32 bytes")
	}
	if cfg.Instance == "" || cfg.LeaseTTL <= 0 || cfg.Heartbeat <= 0 || cfg.ForwardTimeout <= 0 {
		return nil, errors.New("ha: incomplete configuration")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.UsagePersist <= 0 {
		cfg.UsagePersist = 10 * time.Second
	}
	n := &Node{
		cfg: cfg, st: st, client: newRPCClient(cfg.PeerNetworks, cfg.ForwardTimeout, cfg.ClientTLS),
		rpcKey: deriveKey(cfg.Secret, "rpc v1"), replay: newReplayGuard(),
		seqs: map[string]*atomic.Uint64{}, pools: map[string]*poolState{}, kick: make(chan struct{}, 1),
	}
	for _, p := range cfg.Pools {
		n.pools[p.Name] = &poolState{spec: p, local: map[string]*localGrant{}, capacity: p.Capacity, pubCapacity: -1}
		n.order = append(n.order, p.Name)
	}
	return n, nil
}

func (n *Node) nextSeq(addr string) uint64 {
	n.seqMu.Lock()
	c := n.seqs[addr]
	if c == nil {
		c = &atomic.Uint64{}
		n.seqs[addr] = c
	}
	n.seqMu.Unlock()
	return c.Add(1)
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// valid: this node may decide for the pool now.
func (ps *poolState) valid(now time.Time) bool {
	return ps.owner && ps.eng != nil && now.Before(ps.validUntil) && !now.Before(ps.notBefore)
}

func (ps *poolState) observe(epoch int64) {
	if epoch > ps.known {
		ps.known = epoch
	}
}

// stepDown stops deciding (the lease went to someone else, a write was
// fenced, or a peer has seen a newer epoch). The local grants stay: they
// are reported to the next owner.
func (ps *poolState) stepDown() {
	ps.owner, ps.eng = false, nil
}

func (n *Node) lowest(class fairshare.Class) int {
	return fairshare.BorrowBase + fairshare.RankStep*fairshare.MaxRank + int(class)
}

// Acquire admits a request in a pool. With needRoom it is TryAcquire: ok
// is false when the pool has no room, or when its owner cannot say. err is
// ErrNoOwner when no owner answered and RejectUnreachable is set.
func (n *Node) Acquire(pool, org string, class fairshare.Class, tokens int64, needRoom bool) (*fairshare.Grant, bool, error) {
	ps := n.pools[pool]
	if ps == nil {
		return nil, false, errors.New("ha: unknown pool")
	}
	id := newID()
	ps.mu.Lock()
	if ps.valid(time.Now()) {
		g, ok := ps.eng.decide(n.cfg.Instance, id, org, class, tokens, needRoom, time.Now())
		if !ok {
			ps.mu.Unlock()
			return nil, false, nil
		}
		if n.cfg.OnDecide != nil {
			n.cfg.OnDecide(pool, ps.epoch, n.cfg.Instance)
		}
		lg := &localGrant{id: id, org: org, class: class, tokens: tokens, priority: g.Priority, borrowing: g.Borrowing, epoch: ps.epoch}
		ps.local[id] = lg
		ps.mu.Unlock()
		return n.grant(ps, lg, g.Saturated), true, nil
	}
	addr, known := ps.leaseAddr, ps.known
	remote := ps.leaseLive && ps.leaseOwner != n.cfg.Instance && addr != ""
	if remote && ps.mismatch {
		// The owner decides with other shares than this pool's: never serve
		// the pool by it (and never silently by the wrong apportionment).
		ps.mu.Unlock()
		if needRoom {
			return nil, false, nil
		}
		return nil, false, ErrSharesMismatch
	}
	ps.mu.Unlock()
	if remote {
		var resp acquireResp
		err := n.call(context.Background(), addr, pathAcquire, acquireReq{Pool: pool, ID: id, Org: org, Class: int(class), Tokens: tokens, NeedRoom: needRoom, Known: known}, &resp)
		if err == nil {
			ps.mu.Lock()
			if resp.Epoch >= ps.known {
				ps.observe(resp.Epoch)
				if !resp.OK {
					ps.mu.Unlock()
					return nil, false, nil
				}
				lg := &localGrant{id: id, org: org, class: class, tokens: tokens, priority: resp.Priority, borrowing: resp.Borrowing, epoch: resp.Epoch}
				ps.local[id] = lg
				ps.mu.Unlock()
				return n.grant(ps, lg, resp.Saturated), true, nil
			}
			// A stale owner: its answer is refused (it will drop the grant
			// when our report does not list it).
			ps.mu.Unlock()
			n.poke()
		} else {
			n.cfg.Log.Debug("ha: forward failed", "pool", pool, "err", err)
			var se *statusError
			if errors.As(err, &se) && se.status == http.StatusConflict {
				n.poke()
			}
		}
	}
	n.degraded.Add(1)
	if needRoom {
		return nil, false, nil
	}
	if n.cfg.RejectUnreachable {
		return nil, false, ErrNoOwner
	}
	lg := &localGrant{id: id, org: org, class: class, tokens: tokens, priority: n.lowest(class), borrowing: true}
	ps.mu.Lock()
	ps.local[id] = lg
	ps.mu.Unlock()
	return n.grant(ps, lg, false), true, nil
}

func (n *Node) grant(ps *poolState, lg *localGrant, saturated bool) *fairshare.Grant {
	return fairshare.NewRemoteGrant(lg.org, lg.priority, lg.borrowing, saturated, func(consumed float64) {
		n.release(ps, lg, consumed)
	})
}

// release ends a local grant: at once when this node owns the pool,
// otherwise sent to the owner, and kept for the next report when that
// fails.
func (n *Node) release(ps *poolState, lg *localGrant, consumed float64) {
	r := wireRelease{ID: lg.id, Org: lg.org, Consumed: consumed, Epoch: lg.epoch}
	ps.mu.Lock()
	delete(ps.local, lg.id)
	if ps.valid(time.Now()) {
		ps.eng.release(n.cfg.Instance, r, time.Now())
		ps.mu.Unlock()
		return
	}
	addr, known := ps.leaseAddr, ps.known
	remote := ps.leaseLive && ps.leaseOwner != n.cfg.Instance && addr != ""
	if !remote {
		ps.queue(r)
		ps.mu.Unlock()
		return
	}
	ps.mu.Unlock()
	n.life.Lock()
	if n.stopped {
		n.life.Unlock()
		ps.mu.Lock()
		ps.queue(r)
		ps.mu.Unlock()
		return
	}
	n.sends.Add(1)
	n.life.Unlock()
	go func() {
		defer n.sends.Done()
		var resp epochResp
		if err := n.call(context.Background(), addr, pathRelease, releaseReq{Pool: ps.spec.Name, Known: known, Releases: []wireRelease{r}}, &resp); err != nil {
			ps.mu.Lock()
			ps.queue(r)
			ps.mu.Unlock()
		}
	}()
}

func (ps *poolState) queue(r wireRelease) {
	if len(ps.pending) >= maxPending {
		ps.pending = ps.pending[1:]
	}
	ps.pending = append(ps.pending, r)
}

// poke asks the lease loop to look at the leases now.
func (n *Node) poke() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// Start claims the leases once (a lone replica owns its pools from the
// first request) and starts the loops.
func (n *Node) Start(ctx context.Context) {
	for _, name := range n.order {
		n.claim(ctx, n.pools[name])
	}
	bctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.wg.Add(2)
	go func() { defer n.wg.Done(); n.leaseLoop(bctx) }()
	go func() { defer n.wg.Done(); n.reportLoop(bctx) }()
}

// Close stops the loops; an owner persists its usage and ends its leases
// so the next owner need not wait for them to expire.
func (n *Node) Close(ctx context.Context) {
	n.stop()
	for _, name := range n.order {
		ps := n.pools[name]
		ps.mu.Lock()
		valid, epoch := ps.valid(time.Now()), ps.epoch
		var rows []store.PoolUsage
		if valid {
			rows = usageRows(ps.eng)
		}
		ps.stepDown()
		ps.mu.Unlock()
		if valid {
			_ = n.st.SaveUsage(ctx, name, n.cfg.Instance, epoch, rows)
			_ = n.st.ReleaseLease(ctx, name, n.cfg.Instance, epoch)
		}
	}
}

func (n *Node) leaseLoop(ctx context.Context) {
	t := time.NewTicker(n.cfg.LeaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-n.kick:
		}
		for _, name := range n.order {
			n.claim(ctx, n.pools[name])
		}
	}
}

// claim renews the lease this node holds, or takes an expired one.
func (n *Node) claim(ctx context.Context, ps *poolState) {
	ready, fresh := true, true
	if n.cfg.Membership != nil {
		ready, fresh = n.cfg.Membership(ps.spec.Name)
		fresh = fresh && ready
	}
	ps.mu.Lock()
	var held int64
	if ps.owner {
		held = ps.epoch
	}
	c, res := ps.capacity, ps.reservedJSON()
	ps.mu.Unlock()
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, n.cfg.LeaseTTL/3)
	l, err := n.st.ClaimLease(cctx, store.Claim{Pool: ps.spec.Name, Owner: n.cfg.Instance, Addr: n.cfg.Advertise, Held: held, TTL: n.cfg.LeaseTTL,
		MayTake: ready, MayTakeYielded: fresh, Yield: !fresh, Capacity: c, Reserved: res})
	cancel()
	if err != nil {
		// The validity runs out by itself: no renewal, no decisions.
		n.cfg.Log.Warn("ha: lease renewal failed", "pool", ps.spec.Name, "err", err)
		return
	}
	until := start.Add(n.cfg.LeaseTTL * 9 / 10)
	ps.mu.Lock()
	ps.observe(l.Epoch)
	ps.leaseOwner, ps.leaseAddr, ps.leaseEpoch, ps.leaseLive = l.Owner, l.Addr, l.Epoch, l.Live
	ps.pubCapacity, ps.pubReserved = l.Capacity, l.Reserved
	if !l.Live {
		ps.pubCapacity, ps.pubReserved = -1, ""
	}
	// A pool with shares of its own is only decided by an owner that computed
	// its reservations from them (shareMarkers).
	wasMismatch := ps.mismatch
	ps.mismatch = ps.spec.OwnShares && l.Live && l.Owner != n.cfg.Instance && !ownerSharesOK(l.Reserved, SharesFingerprint(ps.spec.Shares))
	if ps.mismatch && !wasMismatch {
		n.cfg.Log.Error("ha: the owner of a pool with shares of its own does not use them; the pool is refused here until every replica runs the same version and configuration",
			"pool", ps.spec.Name, "owner", l.Owner)
	}
	mine := l.Owner == n.cfg.Instance && l.Live
	switch {
	case !mine:
		if ps.owner {
			n.cfg.Log.Warn("ha: pool ownership lost", "pool", ps.spec.Name, "epoch", ps.epoch, "owner", l.Owner, "new_epoch", l.Epoch)
		}
		ps.stepDown()
		ps.mu.Unlock()
		return
	case ps.owner && ps.eng != nil && l.Epoch == ps.epoch:
		ps.validUntil = until
		ps.mu.Unlock()
		return
	case ps.owner && ps.eng != nil && l.Epoch == ps.epoch+1:
		// This node let the lease lapse and took it back: nobody owned
		// the pool in between, so its engine is the latest state.
		ps.epoch, ps.eng.epoch, ps.validUntil = l.Epoch, l.Epoch, until
		ps.mu.Unlock()
		n.cfg.Log.Info("ha: pool ownership resumed", "pool", ps.spec.Name, "epoch", l.Epoch)
		return
	}
	ps.mu.Unlock()
	// A takeover: rebuild from what the previous owner left.
	lctx, cancel := context.WithTimeout(ctx, n.cfg.LeaseTTL/3)
	rows, err := n.st.LoadUsage(lctx, ps.spec.Name)
	cancel()
	if err != nil {
		n.cfg.Log.Warn("ha: cannot read the previous owner's state; not deciding", "pool", ps.spec.Name, "err", err)
		return
	}
	ps.mu.Lock()
	spec := ps.spec
	spec.Capacity = ps.capacity // this replica's own view of the members
	ps.mu.Unlock()
	eng := newEngine(spec, l.Epoch, n.cfg.Now, n.cfg.MaxRequest, n.cfg.Heartbeat)
	wall := n.cfg.Now()
	var margin int
	for _, r := range rows {
		u := r.Usage
		if lam := eng.pool.Lambda(); lam > 0 {
			u *= decay(lam, r.Age)
		}
		eng.pool.SetUsage(r.Org, u)
		eng.pool.AddPhantoms(r.Org, r.InFlight, wall.Add(n.cfg.MaxRequest))
		margin += r.InFlight
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.known > l.Epoch {
		return // someone newer already
	}
	// This node's own grants, as a report.
	eng.report(n.cfg.Instance, ps.localWire(), time.Now())
	eng.pool.SetCapacity(ps.capacity) // changed while the state was read
	ps.owner, ps.epoch, ps.eng, ps.validUntil, ps.lastSave = true, l.Epoch, eng, until, time.Now()
	ps.notBefore = time.Time{}
	if l.Quiet {
		ps.notBefore = time.Now().Add(n.cfg.LeaseTTL)
	}
	n.cfg.Log.Info("ha: pool ownership taken", "pool", ps.spec.Name, "epoch", l.Epoch, "phantoms", margin)
}

// decay is the fraction of usage left after age: e^{-λ·age}, as package
// fairshare decays it.
func decay(lambda float64, age time.Duration) float64 {
	if age <= 0 {
		return 1
	}
	return math.Exp(-lambda * age.Seconds())
}

func (ps *poolState) localWire() []wireGrant {
	out := make([]wireGrant, 0, len(ps.local))
	for _, g := range ps.local {
		out = append(out, wireGrant{ID: g.id, Org: g.org, Class: int(g.class), Tokens: g.tokens, Priority: g.priority, Borrowing: g.borrowing, Epoch: g.epoch})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func usageRows(e *engine) []store.PoolUsage {
	usage, inflight := e.pool.Usage()
	rows := make([]store.PoolUsage, 0, len(usage))
	for org, u := range usage {
		rows = append(rows, store.PoolUsage{Org: org, Usage: u, InFlight: inflight[org]})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Org < rows[j].Org })
	return rows
}

func (n *Node) reportLoop(ctx context.Context) {
	t := time.NewTicker(n.cfg.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, name := range n.order {
			n.heartbeat(ctx, n.pools[name])
		}
	}
}

// heartbeat: an owner applies its own pending releases, expires silent
// replicas and persists its usage when due; any other replica reports its
// grants to the owner.
func (n *Node) heartbeat(ctx context.Context, ps *poolState) {
	now := time.Now()
	ps.mu.Lock()
	if ps.valid(now) {
		for _, r := range ps.pending {
			ps.eng.release(n.cfg.Instance, r, now)
		}
		ps.pending = nil
		ps.eng.tick(n.cfg.Instance, now, n.cfg.Now())
		var rows []store.PoolUsage
		due := now.Sub(ps.lastSave) >= n.cfg.UsagePersist
		if due {
			rows, ps.lastSave = usageRows(ps.eng), now
		}
		epoch := ps.epoch
		ps.mu.Unlock()
		if due {
			sctx, cancel := context.WithTimeout(ctx, n.cfg.LeaseTTL/3)
			err := n.st.SaveUsage(sctx, ps.spec.Name, n.cfg.Instance, epoch, rows)
			cancel()
			if errors.Is(err, store.ErrFenced) {
				ps.mu.Lock()
				if ps.epoch == epoch {
					ps.stepDown()
				}
				ps.mu.Unlock()
				n.cfg.Log.Warn("ha: usage write fenced: stepping down", "pool", ps.spec.Name, "epoch", epoch)
				n.poke()
			} else if err != nil {
				n.cfg.Log.Warn("ha: usage write failed", "pool", ps.spec.Name, "err", err)
			}
		}
		return
	}
	addr, known := ps.leaseAddr, ps.known
	remote := ps.leaseLive && ps.leaseOwner != n.cfg.Instance && addr != ""
	if !remote {
		ps.mu.Unlock()
		return
	}
	req := reportReq{Pool: ps.spec.Name, Known: known, Grants: ps.localWire(), Releases: append([]wireRelease(nil), ps.pending...)}
	ps.mu.Unlock()
	var resp epochResp
	err := n.call(ctx, addr, pathReport, req, &resp)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if err != nil {
		var se *statusError
		if errors.As(err, &se) && se.status == http.StatusConflict {
			n.poke()
		}
		return
	}
	ps.observe(resp.Epoch)
	// The releases sent are delivered; later ones stay.
	if k := len(req.Releases); k <= len(ps.pending) {
		ps.pending = ps.pending[k:]
	}
}

// Status reports whether this node decides for the pool now, and the
// epoch of the lease as it last saw it.
func (n *Node) Status(pool string) (owner bool, epoch int64) {
	ps := n.pools[pool]
	if ps == nil {
		return false, 0
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.valid(time.Now()), ps.known
}

// reservedJSON is the integer reservations of the local C (Apportion: their
// sum never exceeds C), as published with the lease.
func (ps *poolState) reservedJSON() string {
	names := make([]string, 0, len(ps.spec.Shares))
	for k := range ps.spec.Shares {
		names = append(names, k)
	}
	sort.Strings(names)
	res := map[string]int64{}
	for k, v := range fairshare.Apportion(ps.capacity, names, ps.spec.Shares) {
		res[k] = int64(v)
	}
	if ps.spec.OwnShares {
		res[markerOwn], res[markerFP] = 1, SharesFingerprint(ps.spec.Shares)
	}
	b, _ := json.Marshal(res)
	return string(b)
}

// SetCapacity sets the pool's C as this replica's discovery sees it (an
// elastic pool, #75) and returns the previous value. It is what this
// replica decides with when it owns the pool, and what it publishes with
// the lease; another replica's view counts for nothing until it owns the
// pool.
func (n *Node) SetCapacity(pool string, c int) int {
	ps := n.pools[pool]
	if ps == nil {
		return 0
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	old := ps.capacity
	ps.capacity = max(c, 0)
	if ps.eng != nil {
		ps.eng.pool.SetCapacity(ps.capacity)
	}
	return old
}

// Capacity is the pool's C as decided: the engine's on the owner, the
// published one elsewhere, this replica's own view when nothing is
// published.
func (n *Node) Capacity(pool string) int {
	ps := n.pools[pool]
	if ps == nil {
		return 0
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	switch {
	case ps.valid(time.Now()):
		return ps.eng.pool.Capacity()
	case ps.pubCapacity >= 0:
		return ps.pubCapacity
	}
	return ps.capacity
}

// Reserved is the integer reservations as decided (see Capacity).
func (n *Node) Reserved(pool string) map[string]int {
	ps := n.pools[pool]
	if ps == nil {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.valid(time.Now()) {
		return ps.eng.pool.Reserved()
	}
	src := ps.pubReserved
	if ps.pubCapacity < 0 {
		src = ps.reservedJSON()
	}
	var out map[string]int
	_ = json.Unmarshal([]byte(src), &out)
	delete(out, markerOwn)
	delete(out, markerFP)
	return out
}

// Published is the lease as this replica last read it: the owner's epoch,
// and the C and reservations published with it.
func (n *Node) Published(pool string) store.Lease {
	ps := n.pools[pool]
	if ps == nil {
		return store.Lease{}
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return store.Lease{Pool: pool, Owner: ps.leaseOwner, Addr: ps.leaseAddr, Epoch: ps.leaseEpoch, Live: ps.leaseLive,
		Capacity: ps.pubCapacity, Reserved: ps.pubReserved}
}

// Instance names this replica's process.
func (n *Node) Instance() string { return n.cfg.Instance }

// Poke makes the lease loop look at the leases now (the membership became
// known).
func (n *Node) Poke() { n.poke() }

// Snapshot returns the owner's view of a pool (nil on any other replica).
func (n *Node) Snapshot(pool string) []fairshare.TeamStat {
	ps := n.pools[pool]
	if ps == nil {
		return nil
	}
	ps.mu.Lock()
	var p *fairshare.Pool
	if ps.valid(time.Now()) {
		p = ps.eng.pool
	}
	ps.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.Snapshot()
}

// Degraded counts the requests admitted (or refused) without an owner.
func (n *Node) Degraded() int64 { return n.degraded.Load() }

// Local counts this replica's live grants in a pool.
func (n *Node) Local(pool string) int {
	ps := n.pools[pool]
	if ps == nil {
		return 0
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.local)
}

// The internal API.
const (
	pathAcquire = "/ha/v1/acquire"
	pathRelease = "/ha/v1/release"
	pathReport  = "/ha/v1/report"
)

type wireGrant struct {
	ID        string `json:"id"`
	Org       string `json:"org"`
	Class     int    `json:"class"`
	Tokens    int64  `json:"tokens"`
	Priority  int    `json:"priority"`
	Borrowing bool   `json:"borrowing"`
	Epoch     int64  `json:"epoch"`
}

type wireRelease struct {
	ID       string  `json:"id"`
	Org      string  `json:"org"`
	Consumed float64 `json:"consumed"`
	Epoch    int64   `json:"epoch"`
}

type acquireReq struct {
	Pool     string `json:"pool"`
	ID       string `json:"id"`
	Org      string `json:"org"`
	Class    int    `json:"class"`
	Tokens   int64  `json:"tokens"`
	NeedRoom bool   `json:"need_room"`
	Known    int64  `json:"known"`
}

type acquireResp struct {
	Epoch     int64 `json:"epoch"`
	OK        bool  `json:"ok"`
	Priority  int   `json:"priority"`
	Borrowing bool  `json:"borrowing"`
	Saturated bool  `json:"saturated"`
}

type releaseReq struct {
	Pool     string        `json:"pool"`
	Known    int64         `json:"known"`
	Releases []wireRelease `json:"releases"`
}

type reportReq struct {
	Pool     string        `json:"pool"`
	Known    int64         `json:"known"`
	Grants   []wireGrant   `json:"grants"`
	Releases []wireRelease `json:"releases"`
}

type epochResp struct {
	Epoch int64 `json:"epoch"`
}

const maxEntries = 1 << 16

func validName(s string) bool { return s != "" && len(s) <= 256 }

func (w wireGrant) ok() bool {
	return validName(w.ID) && validName(w.Org) && (w.Class == 0 || w.Class == 1) && w.Tokens >= 0 && w.Priority >= 0 && w.Priority < 1000 && w.Epoch >= 0
}

func (r wireRelease) ok() bool { return validName(r.ID) && validName(r.Org) && r.Epoch >= 0 }

// Handler serves the internal API (on its own listener, never on the
// client one).
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+pathAcquire, n.handle(func(inst string, body []byte) (int, any) {
		var req acquireReq
		if json.Unmarshal(body, &req) != nil || !validName(req.ID) || !validName(req.Org) || (req.Class != 0 && req.Class != 1) || req.Tokens < 0 {
			return http.StatusBadRequest, epochResp{}
		}
		ps := n.pools[req.Pool]
		if ps == nil {
			return http.StatusNotFound, epochResp{}
		}
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if st, ok := n.ownerCheck(ps, req.Known); !ok {
			return st, epochResp{Epoch: ps.known}
		}
		g, ok := ps.eng.decide(inst, req.ID, req.Org, fairshare.Class(req.Class), req.Tokens, req.NeedRoom, time.Now())
		if !ok {
			return http.StatusOK, acquireResp{Epoch: ps.epoch}
		}
		if n.cfg.OnDecide != nil {
			n.cfg.OnDecide(req.Pool, ps.epoch, n.cfg.Instance)
		}
		return http.StatusOK, acquireResp{Epoch: ps.epoch, OK: true, Priority: g.Priority, Borrowing: g.Borrowing, Saturated: g.Saturated}
	}))
	mux.HandleFunc("POST "+pathRelease, n.handle(func(inst string, body []byte) (int, any) {
		var req releaseReq
		if json.Unmarshal(body, &req) != nil || len(req.Releases) > maxEntries {
			return http.StatusBadRequest, epochResp{}
		}
		ps := n.pools[req.Pool]
		if ps == nil {
			return http.StatusNotFound, epochResp{}
		}
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if st, ok := n.ownerCheck(ps, req.Known); !ok {
			return st, epochResp{Epoch: ps.known}
		}
		now := time.Now()
		for _, r := range req.Releases {
			if r.ok() {
				ps.eng.release(inst, r, now)
			}
		}
		return http.StatusOK, epochResp{Epoch: ps.epoch}
	}))
	mux.HandleFunc("POST "+pathReport, n.handle(func(inst string, body []byte) (int, any) {
		var req reportReq
		if json.Unmarshal(body, &req) != nil || len(req.Grants) > maxEntries || len(req.Releases) > maxEntries {
			return http.StatusBadRequest, epochResp{}
		}
		ps := n.pools[req.Pool]
		if ps == nil {
			return http.StatusNotFound, epochResp{}
		}
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if st, ok := n.ownerCheck(ps, req.Known); !ok {
			return st, epochResp{Epoch: ps.known}
		}
		now := time.Now()
		live := req.Grants[:0]
		for _, g := range req.Grants {
			if g.ok() {
				live = append(live, g)
			}
		}
		// Releases first: a grant released since the list was built must
		// not be adopted again.
		for _, r := range req.Releases {
			if r.ok() {
				ps.eng.release(inst, r, now)
			}
		}
		ps.eng.report(inst, live, now)
		return http.StatusOK, epochResp{Epoch: ps.epoch}
	}))
	return mux
}

// ownerCheck: 409 when this node does not decide for the pool now, or when
// the caller has seen a newer epoch (this node is stale: it steps down).
func (n *Node) ownerCheck(ps *poolState, known int64) (int, bool) {
	ps.observe(known)
	if ps.owner && known > ps.epoch {
		n.cfg.Log.Warn("ha: a peer has seen a newer epoch: stepping down", "pool", ps.spec.Name, "epoch", ps.epoch, "seen", known)
		ps.stepDown()
		n.poke()
	}
	if !ps.valid(time.Now()) {
		return http.StatusConflict, false
	}
	return 0, true
}

func (n *Node) handle(f func(inst string, body []byte) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, inst, seq, err := n.verify(r)
		if err != nil {
			// Unsigned: nothing to bind a signature to.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		st, v := f(inst, body)
		n.reply(w, inst, seq, st, v)
	}
}
