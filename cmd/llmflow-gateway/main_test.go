package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := RootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestVersion(t *testing.T) {
	out, err := run(t, "version")
	if err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("%q %v", out, err)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckConfig(t *testing.T) {
	dir := t.TempDir()
	mk := writeFile(t, dir, "master", "m-secret\n")
	vk := writeFile(t, dir, "vllm", "v-secret\n")
	cfg := writeFile(t, dir, "c.hcl", `
master_key_file = "`+mk+`"
team "a" { share = 1 }
pool "p" { capacity = 4 }
model "m" {
  pool     = "p"
  key_file = "`+vk+`"
  upstream "u" { base_url = "http://127.0.0.1:1" }
}
`)
	out, err := run(t, "check-config", "--config", cfg)
	if err != nil || !strings.Contains(out, "ok: 1 teams") {
		t.Fatalf("%q %v", out, err)
	}
	if strings.Contains(out, "secret") {
		t.Fatal("secret printed")
	}
	if err := os.Remove(vk); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "check-config", "-c", cfg); err == nil {
		t.Fatal("missing key file accepted")
	}
	if _, err := run(t, "check-config"); err == nil {
		t.Fatal("missing --config accepted")
	}
	// Diagnostics carry file:line:column.
	bad := writeFile(t, dir, "bad.hcl", "master_key_file = \"/m\"\npool \"p\" { capacity = 0 }\n")
	if _, err := run(t, "check-config", "-c", bad); err == nil || !strings.Contains(err.Error(), "bad.hcl:2:23: Invalid capacity") {
		t.Fatalf("diagnostic: %v", err)
	}
}
