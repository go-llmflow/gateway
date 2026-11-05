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
	if c.Fluids != nil && (len(c.Fluids.FixedCosts) != 4 || c.Fluids.FixedCosts[0].MonthlyFor("2026-12") != 1950 ||
		c.Fluids.FixedCosts[1].Charge != ChargeNonShareholders || c.Fluids.FixedCosts[2].Basis != BasisStorageGBMonth || c.Fluids.FixedCosts[3].Basis != BasisNetworkBytes) {
		t.Fatalf("fixed costs: %+v", c.Fluids.FixedCosts)
	}
	if c.MarkupOf("tiers") != 0.15 || c.Fluids.PricePerGB != 0.02 || c.Fluids.PricePerGBMonth != 0.05 {
		t.Fatalf("markup %v prices %v %v", c.MarkupOf("tiers"), c.Fluids.PricePerGB, c.Fluids.PricePerGBMonth)
	}
	if c.Fluids == nil || c.Fluids.IdleEnergy != "by_usage" {
		t.Fatalf("fluids: %+v", c.Fluids)
	}
}
