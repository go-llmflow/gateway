package fairshare

import (
	"fmt"
	"testing"
)

// A team with no share (a third party lent spare capacity) is served after
// every shareholder, however many distinct usage levels the shareholders
// have. With the old clamp (rank <= MaxRank for everyone) ten shareholders
// with distinct usages ranked 0..9 and the zero-share team, ranked last
// (10), was clamped to 9: it tied with the most over-served owner.
func TestZeroShareTeamIsAlwaysLastBand(t *testing.T) {
	const n = 10
	shares := map[string]float64{"tiers": 0}
	for i := 0; i < n; i++ {
		shares[fmt.Sprintf("o%d", i)] = 1.0 / n
	}
	p := NewPool("b300", n, shares, 0, nil)
	// Distinct usages: owner i consumed i+1 units.
	for i := 0; i < n; i++ {
		g := p.Acquire(fmt.Sprintf("o%d", i), Interactive, 1)
		g.Release(float64(i + 1))
	}
	maxOwner := -1
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("o%d", i)
		p.Acquire(name, Interactive, 1) // takes the within-share slot
		g := p.Acquire(name, Interactive, 1)
		if !g.Borrowing {
			t.Fatalf("%s: second request is not borrowing", name)
		}
		if g.Priority > maxOwner {
			maxOwner = g.Priority
		}
	}
	g := p.Acquire("tiers", Interactive, 1)
	if !g.Borrowing || g.Priority != BorrowBase+RankStep*MaxRank {
		t.Fatalf("zero-share team: priority %d, want the last band %d", g.Priority, BorrowBase+RankStep*MaxRank)
	}
	if g.Priority <= maxOwner {
		t.Fatalf("zero-share priority %d is not strictly greater than the most over-served owner's %d", g.Priority, maxOwner)
	}
	if maxOwner != BorrowBase+RankStep*(MaxRank-1) {
		t.Errorf("shareholders clamp at %d, got %d", BorrowBase+RankStep*(MaxRank-1), maxOwner)
	}
	// Batch of the zero-share team stays in the last band, after interactive.
	if b := p.Acquire("tiers", Batch, 1); b.Priority != g.Priority+1 {
		t.Errorf("batch priority %d, want %d", b.Priority, g.Priority+1)
	}
}

// With no share anywhere (every team at 0), nobody is demoted below the
// others: the ranking is by usage as before.
func TestAllZeroShareKeepsRanking(t *testing.T) {
	p := NewPool("p", 4, map[string]float64{"a": 0, "b": 0}, 0, nil)
	if g := p.Acquire("a", Interactive, 1); g.Priority >= BorrowBase+RankStep*MaxRank {
		t.Fatalf("priority %d", g.Priority)
	}
}
