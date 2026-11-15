package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// gateway#176: the start wait discarded the child's stderr, so a replica that
// did not print "listening" within the limit left nothing to read. launchGateway
// must name what the child said, return at once when the child is gone, and
// report a timeout with the time it waited.
func fakeGateway(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stand-in for the gateway")
	}
	p := filepath.Join(t.TempDir(), "fake-gateway")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLaunchGatewayChildExitsEarly(t *testing.T) {
	bin := fakeGateway(t, "echo 'boom on stderr' >&2; echo 'boom on stdout'; exit 3")
	begin := time.Now()
	_, _, _, err := launchGateway(bin, "cfg", time.Minute)
	if err == nil {
		t.Fatal("a child that exits before listening must be an error")
	}
	if time.Since(begin) > 30*time.Second {
		t.Fatalf("waited %v for a child that was already gone", time.Since(begin))
	}
	for _, want := range []string{"exited", "boom on stderr", "boom on stdout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestLaunchGatewayTimeoutKeepsOutput(t *testing.T) {
	bin := fakeGateway(t, "echo 'migrating' >&2; exec sleep 30")
	_, _, _, err := launchGateway(bin, "cfg", 500*time.Millisecond)
	if err == nil {
		t.Fatal("a child that never listens must be an error")
	}
	for _, want := range []string{"not listening after 500ms", "migrating"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestLaunchGatewayListening(t *testing.T) {
	bin := fakeGateway(t, "echo 'listening on 127.0.0.1:9'; exec sleep 30")
	cmd, addr, took, err := launchGateway(bin, "cfg", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if addr != "127.0.0.1:9" || took <= 0 {
		t.Fatalf("addr %q took %v", addr, took)
	}
}
