package config

import (
	"os"
	"path/filepath"
	"strings"
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
	if n := testing.AllocsPerRun(1000, func() { _ = c.PoolSpareOnly("b300", "etabC") }); n != 0 {
		t.Fatalf("PoolSpareOnly allocates %v times per call", n)
	}
}

// borrow = "spare": only an organisation with no share of the pool is served
// from spare capacity only (for a pool without shares of its own, the
// organisation's own share decides). Unknown values are refused.
func TestPoolSpareOnly(t *testing.T) {
	src := `
master_key_file = "/unused"
cache_salt_key_file = "/unused"
org "owner" {
  share = 1
  team "owner" {}
}
org "tiers" {
  share = 0
  team "tiers" {}
}
pool "own" { capacity = 4 }
pool "b300" {
  capacity = 8
  shares   = { owner = 1, tiers = 0 }
  borrow   = "spare"
}
pool "ownspare" {
  capacity = 8
  borrow   = "spare"
}
model "m" {
  pool     = "b300"
  key_file = "/unused"
  upstream "u" { base_url = "http://x" }
}
`
	c, err := Parse([]byte(src), "spare.hcl")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pool, org string
		want      bool
	}{
		{"b300", "tiers", true}, {"b300", "owner", false}, {"b300", "unknown", true},
		{"ownspare", "tiers", true}, {"ownspare", "owner", false},
		{"own", "tiers", false}, {"nope", "tiers", false},
	} {
		if got := c.PoolSpareOnly(tc.pool, tc.org); got != tc.want {
			t.Errorf("PoolSpareOnly(%q, %q) = %v, want %v", tc.pool, tc.org, got, tc.want)
		}
	}
	if !c.PoolAllows("b300", "tiers") {
		t.Error("spare is not a refusal at the door")
	}
	bad := []byte(strings.ReplaceAll(src, `"spare"`, `"sparse"`))
	if _, err := Parse(bad, "bad.hcl"); err == nil || !contains(err.Error(), "spare") {
		t.Errorf("an unknown borrow value: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
