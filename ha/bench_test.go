package ha

import (
	"testing"

	"github.com/go-llmflow/gateway/fairshare"
)

// The latency the HA mode adds to an admission: an acquire and its release
// decided in process (the owner, or one replica without ha) against one
// forwarded to the owner over the internal channel (loopback here; add the
// network's round trip between hosts). The release is asynchronous on a
// non-owner, so the client waits for the acquire only.
func BenchmarkAdmission(b *testing.B) {
	st := NewMemStore(nil)
	owner := startMember(b, st, "owner", nil)
	other := startMember(b, st, "other", nil)
	local := fairshare.NewPool("p", 4, map[string]float64{"a": 0.5, "b": 0.5}, 0, nil)
	b.Run("single-replica", func(b *testing.B) {
		for b.Loop() {
			local.Acquire("a", fairshare.Interactive, 100).Release(0.001)
		}
	})
	b.Run("owner", func(b *testing.B) {
		for b.Loop() {
			g, _, _ := owner.n.Acquire("p", "a", fairshare.Interactive, 100, false)
			g.Release(0.001)
		}
	})
	b.Run("forwarded", func(b *testing.B) {
		for b.Loop() {
			g, _, _ := other.n.Acquire("p", "a", fairshare.Interactive, 100, false)
			g.Release(0.001)
		}
		if other.n.Degraded() != 0 {
			b.Fatal("admissions were not forwarded")
		}
	})
}
