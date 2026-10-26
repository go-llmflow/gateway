package ha

import (
	"context"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// MemStore is an in-memory store.HAStore with the semantics of the
// PostgreSQL one (store/ha.go): one clock (the "server's"), conditional
// takeover with epoch + 1, fenced usage writes. For tests; Fail, when
// set, makes every call fail (a partition between a replica and the
// database is a MemStore view per replica, see View).
type MemStore struct {
	mu     sync.Mutex
	now    func() time.Time
	leases map[string]*memLease
	usage  map[string]map[string]memUsage
}

type memLease struct {
	owner, addr string
	epoch       int64
	expires     time.Time
	yield       bool
	capacity    int
	reserved    string
}

type memUsage struct {
	usage    float64
	inflight int
	epoch    int64
	at       time.Time
}

// NewMemStore returns an empty store on the given clock (time.Now if nil).
func NewMemStore(now func() time.Time) *MemStore {
	if now == nil {
		now = time.Now
	}
	return &MemStore{now: now, leases: map[string]*memLease{}, usage: map[string]map[string]memUsage{}}
}

func (m *MemStore) ClaimLease(_ context.Context, c store.Claim) (store.Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	l := m.leases[c.Pool]
	if l == nil {
		l = &memLease{capacity: -1}
		m.leases[c.Pool] = l
	}
	live := now.Before(l.expires)
	next, quiet := int64(-1), false
	switch {
	case l.owner == c.Owner && l.epoch == c.Held && c.Held > 0 && live:
		next = l.epoch
	case !live && (c.MayTake || l.owner == c.Owner && l.epoch == c.Held && c.Held > 0):
		next = l.epoch + 1
	case live && l.yield && l.owner != c.Owner && c.MayTakeYielded:
		next, quiet = l.epoch+1, true
	}
	if next >= 0 {
		l.owner, l.addr, l.epoch, l.expires = c.Owner, c.Addr, next, now.Add(c.TTL)
		l.yield, l.capacity, l.reserved = c.Yield, c.Capacity, c.Reserved
	}
	return store.Lease{Pool: c.Pool, Owner: l.owner, Addr: l.addr, Epoch: l.epoch, Live: now.Before(l.expires),
		Yield: l.yield, Quiet: quiet, Capacity: l.capacity, Reserved: l.reserved}, nil
}

func (m *MemStore) ReleaseLease(_ context.Context, pool, owner string, epoch int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.leases[pool]; l != nil && l.owner == owner && l.epoch == epoch && m.now().Before(l.expires) {
		l.expires = m.now()
	}
	return nil
}

func (m *MemStore) SaveUsage(_ context.Context, pool, owner string, epoch int64, rows []store.PoolUsage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.leases[pool]
	if l == nil || l.owner != owner || l.epoch != epoch || !m.now().Before(l.expires) {
		return store.ErrFenced
	}
	u := m.usage[pool]
	if u == nil {
		u = map[string]memUsage{}
		m.usage[pool] = u
	}
	for _, r := range rows {
		if old, ok := u[r.Org]; ok && old.epoch > epoch {
			continue
		}
		u[r.Org] = memUsage{usage: r.Usage, inflight: r.InFlight, epoch: epoch, at: m.now()}
	}
	return nil
}

func (m *MemStore) LoadUsage(_ context.Context, pool string) ([]store.PoolUsage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.PoolUsage
	for org, u := range m.usage[pool] {
		out = append(out, store.PoolUsage{Org: org, Usage: u.usage, InFlight: u.inflight, Age: max(m.now().Sub(u.at), 0)})
	}
	return out, nil
}

// Lease returns a pool's lease (tests).
func (m *MemStore) Lease(pool string) store.Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.leases[pool]
	if l == nil {
		return store.Lease{Pool: pool}
	}
	return store.Lease{Pool: pool, Owner: l.owner, Addr: l.addr, Epoch: l.epoch, Live: m.now().Before(l.expires)}
}

// View is one replica's access to a store, which a test can cut (a
// network partition between that replica and the database).
type View struct {
	store.HAStore
	mu  sync.Mutex
	cut bool
}

// NewView wraps st.
func NewView(st store.HAStore) *View { return &View{HAStore: st} }

// Cut makes every call fail (true) or pass again (false).
func (v *View) Cut(cut bool) {
	v.mu.Lock()
	v.cut = cut
	v.mu.Unlock()
}

func (v *View) err() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cut {
		return errPartition
	}
	return nil
}

var errPartition = &partitionError{}

type partitionError struct{}

func (*partitionError) Error() string { return "ha: partitioned from the database (test)" }

func (v *View) ClaimLease(ctx context.Context, c store.Claim) (store.Lease, error) {
	if err := v.err(); err != nil {
		return store.Lease{}, err
	}
	return v.HAStore.ClaimLease(ctx, c)
}

func (v *View) ReleaseLease(ctx context.Context, pool, owner string, epoch int64) error {
	if err := v.err(); err != nil {
		return err
	}
	return v.HAStore.ReleaseLease(ctx, pool, owner, epoch)
}

func (v *View) SaveUsage(ctx context.Context, pool, owner string, epoch int64, rows []store.PoolUsage) error {
	if err := v.err(); err != nil {
		return err
	}
	return v.HAStore.SaveUsage(ctx, pool, owner, epoch, rows)
}

func (v *View) LoadUsage(ctx context.Context, pool string) ([]store.PoolUsage, error) {
	if err := v.err(); err != nil {
		return nil, err
	}
	return v.HAStore.LoadUsage(ctx, pool)
}
