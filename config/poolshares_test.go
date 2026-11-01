package config

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/fairshare"
)

const poolsBase = `
master_key_file = "/m"
cache_salt_key_file = "/c"
org "a" {
  share = 0.5
  team "ta" {}
}
org "b" {
  share = 0.5
  team "tb" {}
}
org "c" {
  share = 0
  team "tc" {}
}
`

func poolsHCL(pools string) string {
	return poolsBase + pools + `
model "m" {
  pool = "p"
  key_file = "/k"
  upstream "u" { base_url = "http://127.0.0.1:1" }
}
`
}

// The example of the README: parsed by check-config.
func TestTwoMachinesExample(t *testing.T) {
	c, err := Load(filepath.Join("..", "examples", "two-machines.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	// B300: the organisations' shares. RTX: its own, by largest remainder.
	b300 := c.PoolShares("b300")
	if b300["etabA"] != 0.5 || b300["etabB"] != 0.25 || b300["etabD"] != 0 {
		t.Errorf("b300 %v", b300)
	}
	rtx := c.PoolShares("rtx6000")
	if len(rtx) != 3 || rtx["etabA"] != 0.5 || rtx["etabC"] != 0.3 || rtx["etabD"] != 0.2 || rtx["etabB"] != 0 {
		t.Errorf("rtx %v", rtx)
	}
	names := []string{"etabA", "etabB", "etabC", "etabD"}
	got := fairshare.Apportion(64, names, rtx)
	if got["etabA"] != 32 || got["etabC"] != 19 || got["etabD"] != 13 || got["etabB"] != 0 {
		t.Errorf("reservations over C = 64: %v, want 32/0/19/13 (largest remainder: 32, 19.2, 12.8)", got)
	}
	b := fairshare.Apportion(256, names, b300)
	if b["etabA"] != 128 || b["etabB"] != 64 || b["etabC"] != 64 || b["etabD"] != 0 {
		t.Errorf("b300 reservations %v", b)
	}
	if c.PoolBorrow("rtx6000") != BorrowShareholders || c.PoolBorrow("b300") != BorrowAll {
		t.Errorf("borrow %q %q", c.PoolBorrow("rtx6000"), c.PoolBorrow("b300"))
	}
	if c.PoolAllows("rtx6000", "etabB") || !c.PoolAllows("rtx6000", "etabD") || !c.PoolAllows("b300", "etabD") {
		t.Error("PoolAllows")
	}
	if !c.HasPoolShares("rtx6000") || c.HasPoolShares("b300") || strings.Join(c.PoolsWithShares(), ",") != "rtx6000" {
		t.Error("HasPoolShares")
	}
}

// A pool without shares means what it meant: the organisations' shares.
func TestPoolWithoutSharesKeepsOrgShares(t *testing.T) {
	c, err := Parse([]byte(poolsHCL(`pool "p" { capacity = 10 }`)), "x.hcl")
	if err != nil {
		t.Fatal(err)
	}
	s := c.PoolShares("p")
	if s["a"] != 0.5 || s["b"] != 0.5 || s["c"] != 0 || len(s) != 3 || c.HasPoolShares("p") || c.PoolBorrow("p") != BorrowAll {
		t.Fatalf("%v", s)
	}
	// An unknown pool name (idle energy of no known pool) is the same.
	if x := c.PoolShares("nope"); x["a"] != 0.5 {
		t.Fatalf("%v", x)
	}
	// The returned map is the caller's.
	s["a"] = 9
	if c.PoolShares("p")["a"] != 0.5 {
		t.Fatal("PoolShares returned the configuration's own map")
	}
}

func TestPoolSharesValidation(t *testing.T) {
	bad := map[string]string{
		`pool "p" {
  capacity = 10
  shares = { a = 0.5, zzz = 0.5 }
}`: "not declared in an org block",
		`pool "p" {
  capacity = 10
  shares = { a = 0.7, b = 0.7 }
}`: "Shares exceed the pool",
		`pool "p" {
  capacity = 10
  shares = { a = 1.5 }
}`: "want 0..1",
		`pool "p" {
  capacity = 10
  shares = { a = -0.1 }
}`: "want 0..1",
		`pool "p" {
  capacity = 10
  shares = {}
}`: "Empty shares",
		`pool "p" {
  capacity = 10
  borrow = "nobody"
}`: "Invalid borrow",
		`pool "p" {
  capacity = 10
  shares = { a = "half" }
}`: "",
	}
	for src, want := range bad {
		if _, err := Parse([]byte(poolsHCL(src)), "x.hcl"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", src, err, want)
		}
	}
	ok := `pool "p" {
  capacity = 10
  shares = { a = 0.5, b = 0.5, c = 0 }
  borrow = "shareholders"
}`
	c, err := Parse([]byte(poolsHCL(ok)), "x.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if c.PoolAllows("p", "c") || !c.PoolAllows("p", "a") {
		t.Error("a share of 0 is not a shareholder")
	}
	// A partial sum is fine (a machine not fully allocated).
	if _, err := Parse([]byte(poolsHCL(`pool "p" {
  capacity = 10
  shares = { a = 0.3 }
}`)), "x.hcl"); err != nil {
		t.Errorf("a partial apportionment: %v", err)
	}
}

// A DCGM node serves one pool when a pool has shares of its own.
func TestDCGMNodeBelongsToOnePool(t *testing.T) {
	src := poolsBase + `
fluids {
  pue = 1
}
pool "p" {
  capacity = 10
  shares = { a = 0.5, b = 0.5 }
}
pool "q" { capacity = 10 }
model "m" {
  pool = "p"
  key_file = "/k"
  upstream "u" {
    base_url = "http://127.0.0.1:1"
    dcgm_url = "http://127.0.0.1:9400/metrics"
  }
}
model "n" {
  pool = "q"
  key_file = "/k"
  upstream "u" {
    base_url = "http://127.0.0.1:2"
    dcgm_url = "http://127.0.0.1:9400/metrics"
  }
}
`
	if _, err := Parse([]byte(src), "x.hcl"); err == nil || !strings.Contains(err.Error(), "dcgm_url shared by two pools") {
		t.Fatalf("%v", err)
	}
	// Two pools without shares of their own may share a node, as before.
	ok := strings.Replace(src, "  shares = { a = 0.5, b = 0.5 }\n", "", 1)
	if _, err := Parse([]byte(ok), "x.hcl"); err != nil {
		t.Fatal(err)
	}
}

// The new HCL block must not panic or loop on any input, and what it accepts
// obeys the rules the validation states.
func FuzzPoolShares(f *testing.F) {
	for _, s := range []string{
		`pool "p" { capacity = 4 }`,
		`pool "p" {
  capacity = 4
  shares = { a = 0.5, b = 0.5 }
  borrow = "shareholders"
}`,
		`pool "p" { capacity = 4  shares = {} }`,
		`pool "p" { capacity = 4  shares = { a = 1e999 } }`,
		`pool "p" { capacity = 4  shares = { a = "x" } }`,
		`pool "p" { capacity = 4  shares = { "a b" = 0.1 } }`,
		`pool "p" { capacity = 4  borrow = 3 }`,
		`pool "p" { capacity = 4  shares = [1] }`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, pools string) {
		if len(pools) > 4096 {
			return
		}
		c, err := Parse([]byte(poolsHCL(pools)), "fuzz.hcl")
		if err != nil {
			return
		}
		for _, p := range c.Pools {
			sum := 0.0
			for o, s := range p.Shares {
				if !(s >= 0 && s <= 1) || math.IsNaN(s) {
					t.Fatalf("pool %s: share %v of %s accepted", p.Name, s, o)
				}
				if o != "a" && o != "b" && o != "c" {
					t.Fatalf("pool %s: unknown org %q accepted", p.Name, o)
				}
				sum += s
			}
			if sum > 1+1e-9 {
				t.Fatalf("pool %s: shares sum to %v", p.Name, sum)
			}
			if p.Borrow != BorrowAll && p.Borrow != BorrowShareholders {
				t.Fatalf("pool %s: borrow %q", p.Name, p.Borrow)
			}
			// The reservations never exceed C.
			names := []string{"a", "b", "c"}
			total := 0
			for _, n := range fairshare.Apportion(max(p.Capacity, 0), names, c.PoolShares(p.Name)) {
				total += n
			}
			if total > p.Capacity {
				t.Fatalf("pool %s: %d slots reserved of %d", p.Name, total, p.Capacity)
			}
		}
	})
}
