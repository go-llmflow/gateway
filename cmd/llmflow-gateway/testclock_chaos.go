//go:build chaosclock

package main

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/gateway/gateway"
)

const testClockEnv = "LLMFLOW_TEST_CLOCK_FILE"

// movedClock is time.Now() plus an offset that a file holds, in seconds
// (negative allowed), re-read every 20 ms. It exists in a chaos build only: a
// DST change, an NTP step or a month boundary cannot otherwise be reached from
// outside the process.
type movedClock struct{ offsetNs atomic.Int64 }

func (c *movedClock) now() time.Time { return time.Now().Add(time.Duration(c.offsetNs.Load())) }

func readOffset(path string) (time.Duration, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(s * float64(time.Second)), true
}

func testClockOptions(log *slog.Logger) ([]gateway.Option, error) {
	path := os.Getenv(testClockEnv)
	if path == "" {
		return nil, nil
	}
	c := &movedClock{}
	if d, ok := readOffset(path); ok {
		c.offsetNs.Store(int64(d))
	}
	go func() {
		for range time.Tick(20 * time.Millisecond) {
			if d, ok := readOffset(path); ok {
				c.offsetNs.Store(int64(d))
			}
		}
	}()
	log.Warn("TEST CLOCK: this binary was built with -tags chaosclock and its clock is moved by a file; never run it in production", "file", path)
	return []gateway.Option{gateway.WithClock(c.now)}, nil
}
