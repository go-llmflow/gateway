//go:build !chaosclock

package main

import (
	"errors"
	"log/slog"
	"os"

	"github.com/go-llmflow/gateway/gateway"
)

// testClockEnv names a file holding a clock offset, honoured only by a binary
// built with -tags chaosclock (see testclock_chaos.go): a chaos harness moves
// the gateway's clock by writing it.
const testClockEnv = "LLMFLOW_TEST_CLOCK_FILE"

// testClockOptions: a production build has no clock seam. Asking for one is
// refused, never silently ignored: a harness that believed it had moved the
// clock would judge nothing.
func testClockOptions(*slog.Logger) ([]gateway.Option, error) {
	if os.Getenv(testClockEnv) != "" {
		return nil, errors.New(testClockEnv + " is set, but this binary is not built with -tags chaosclock: the gateway's clock cannot be moved")
	}
	return nil, nil
}
