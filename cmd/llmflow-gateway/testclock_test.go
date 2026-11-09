//go:build !chaosclock

package main

import (
	"log/slog"
	"strings"
	"testing"
)

func TestAProductionBuildRefusesAClockFile(t *testing.T) {
	t.Setenv(testClockEnv, "/some/file")
	if _, err := testClockOptions(slog.Default()); err == nil || !strings.Contains(err.Error(), "chaosclock") {
		t.Fatalf("err = %v: a production binary must refuse the test clock, not ignore it", err)
	}
	t.Setenv(testClockEnv, "")
	if o, err := testClockOptions(slog.Default()); err != nil || len(o) != 0 {
		t.Fatalf("without the variable: %v %v", o, err)
	}
}
