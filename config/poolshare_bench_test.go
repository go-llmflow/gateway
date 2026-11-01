package config

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkPoolAllows times the per-request pool check on the two-machine
// example.
func BenchmarkPoolAllows(b *testing.B) {
	src, err := os.ReadFile(filepath.Join("..", "examples", "two-machines.hcl"))
	if err != nil {
		b.Fatal(err)
	}
	c, err := Parse(src, "two-machines.hcl")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = c.PoolAllows("rtx6000", "etabC")
	}
}
