package config

import (
	"os"
	"path/filepath"
	"testing"
)

// PoolShare answers what PoolShares answers, for every pool (with and
// without its own shares, and an unknown one) and every organisation (with
// a share, without one, and an unknown one), and PoolAllows, which runs on
// every request, allocates nothing. (The map PoolShares builds did not
// escape either: this test passes on the old code too. The direct lookup
// is about 5 times faster, BenchmarkPoolAllows, but that is ~50 ns of the
// 4.7 µs gwbench measured between v0.6.0 and v0.24.0 on 2026-11-01, not its
// cause.)
func TestPoolShareMatchesPoolSharesWithoutAllocating(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "examples", "two-machines.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(src, "two-machines.hcl")
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range []string{"b300", "rtx6000", "no-such-pool"} {
		all := c.PoolShares(pool)
		for _, org := range []string{"etabA", "etabB", "etabC", "etabD", "no-such-org"} {
			if got, want := c.PoolShare(pool, org), all[org]; got != want {
				t.Errorf("PoolShare(%q, %q) = %v, PoolShares says %v", pool, org, got, want)
			}
		}
	}
	if !c.PoolAllows("rtx6000", "etabA") || c.PoolAllows("rtx6000", "etabB") || !c.PoolAllows("b300", "etabD") {
		t.Fatal("PoolAllows changed meaning")
	}
	if n := testing.AllocsPerRun(1000, func() { _ = c.PoolAllows("rtx6000", "etabC") }); n != 0 {
		t.Fatalf("PoolAllows allocates %v times per call", n)
	}
}
