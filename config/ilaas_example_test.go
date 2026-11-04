package config

import (
	"path/filepath"
	"testing"
)

// The ILaaS example parses and means what its header says.
func TestILaaSExample(t *testing.T) {
	c, err := Load(filepath.Join("..", "examples", "b300-ilaas.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	if c.PoolBorrow("b300") != BorrowSpare {
		t.Fatalf("borrow = %q", c.PoolBorrow("b300"))
	}
	if !c.PoolSpareOnly("b300", "tiers") || c.PoolSpareOnly("b300", "etabA") {
		t.Fatal("tiers is the only spare-only organisation")
	}
	if c.Fluids == nil || c.Fluids.IdleEnergy != "by_share" {
		t.Fatalf("fluids: %+v", c.Fluids)
	}
}
