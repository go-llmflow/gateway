//go:build chaosclock

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheChaosClockFollowsItsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "clock")
	if err := os.WriteFile(f, []byte("3600\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(testClockEnv, f)
	c := &movedClock{}
	d, ok := readOffset(f)
	if !ok || d != time.Hour {
		t.Fatalf("offset %v %v", d, ok)
	}
	c.offsetNs.Store(int64(d))
	if got := time.Until(c.now()); got < 59*time.Minute || got > 61*time.Minute {
		t.Fatalf("clock is %v ahead, want 1 h", got)
	}
	if o, err := testClockOptions(slog.Default()); err != nil || len(o) != 1 {
		t.Fatalf("options %v %v", o, err)
	}
	if err := os.WriteFile(f, []byte("-7200"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
}
