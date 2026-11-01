package ha

import (
	"errors"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/fairshare"
)

func ownShares(shares map[string]float64) func(*Config) {
	return func(c *Config) {
		c.Pools = []PoolSpec{{Name: "p", Capacity: 4, Shares: shares, OwnShares: true, HalfLife: time.Hour}}
	}
}

// A replica from before per-pool shares owns the pool and decides with the
// organisations' shares: it publishes no marker. A replica whose pool has
// shares of its own refuses to follow it, instead of serving the pool with the
// wrong apportionment. (A replica of the old version is the default member:
// OwnShares false publishes exactly what it published.)
func TestOldOwnerIsRefusedForAPoolWithOwnShares(t *testing.T) {
	st := NewMemStore(nil)
	old := startMember(t, st, "old", nil) // org shares a, b: 0.5/0.5, no marker
	fresh := startMember(t, st, "new", ownShares(map[string]float64{"a": 0.25, "b": 0.75}))
	if !owner(old) || owner(fresh) {
		t.Fatal("the first replica started should own the pool")
	}
	eventually(t, 2*time.Second, "the new replica sees the old owner's lease", func() bool {
		_, _, err := fresh.n.Acquire("p", "a", fairshare.Interactive, 100, false)
		return errors.Is(err, ErrSharesMismatch)
	})
	if g, ok, err := fresh.n.Acquire("p", "a", fairshare.Interactive, 100, true); g != nil || ok || err != nil {
		t.Fatalf("TryAcquire on a mismatching owner: %v %v %v", g, ok, err)
	}
	// The old replica itself, owner, serves its own requests as before.
	if g := acquire(t, old, "a"); g == nil {
		t.Fatal("the old owner refuses its own requests")
	}
}

// The same pool on a replica without own shares keeps working against a
// replica that has: an old replica is not the one that must refuse. And two
// new replicas with the same shares agree; with other shares they do not.
func TestOwnSharesMarkerAgrees(t *testing.T) {
	shares := map[string]float64{"a": 0.25, "b": 0.75}
	st := NewMemStore(nil)
	m1 := startMember(t, st, "r1", ownShares(shares))
	m2 := startMember(t, st, "r2", ownShares(map[string]float64{"a": 0.25, "b": 0.75}))
	m3 := startMember(t, st, "r3", ownShares(map[string]float64{"a": 0.5, "b": 0.5}))
	if !owner(m1) {
		t.Fatal("r1 should own the pool")
	}
	// Same shares: forwarded to the owner, whose reservation is 1 of 4 for a.
	g1 := acquire(t, m2, "a")
	if g1.Borrowing {
		t.Fatal("a's first request is within its share of 1")
	}
	g2 := acquire(t, m2, "a")
	if !g2.Borrowing {
		t.Fatal("a's second request exceeds its reservation of 1 (0.25 × 4)")
	}
	// Other shares: refused.
	eventually(t, 2*time.Second, "r3 sees the mismatch", func() bool {
		_, _, err := m3.n.Acquire("p", "a", fairshare.Interactive, 100, false)
		return errors.Is(err, ErrSharesMismatch)
	})
	// An old replica reading the owner's reservations sees organisations only
	// to the extent of its own configuration; this replica strips the markers.
	res := m2.n.Reserved("p")
	if _, ok := res[markerOwn]; ok {
		t.Fatalf("markers leaked into the reservations: %v", res)
	}
	if res["a"] != 1 || res["b"] != 3 {
		t.Fatalf("reservations %v, want a=1 b=3", res)
	}
}

func TestSharesFingerprint(t *testing.T) {
	a := SharesFingerprint(map[string]float64{"a": 0.5, "b": 0.5})
	if a != SharesFingerprint(map[string]float64{"b": 0.5, "a": 0.5}) {
		t.Fatal("the fingerprint depends on map order")
	}
	if a == SharesFingerprint(map[string]float64{"a": 0.5, "b": 0.25}) || a == SharesFingerprint(map[string]float64{"a": 0.5, "c": 0.5}) {
		t.Fatal("different shares, same fingerprint")
	}
	if a < 0 || a >= 1<<52 {
		t.Fatalf("fingerprint %d out of 52 bits", a)
	}
	if ownerSharesOK(`{"a":1,"b":3}`, a) || ownerSharesOK(`not json`, a) || !ownerSharesOK(`{"a":1,"@own":1,"@fp":`+itoa(a)+`}`, a) || ownerSharesOK(`{"a":1,"@own":1,"@fp":1}`, a) {
		t.Fatal("ownerSharesOK")
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
