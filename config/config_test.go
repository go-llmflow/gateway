package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
master_key_file: /etc/llmflow/master.key
teams:
  - {name: etabA, share: 0.8125}
  - {name: etabB, share: 0.1875}
pools:
  - {name: b300, capacity: 256}
model_groups:
  - name: llama
    pool: b300
    upstreams: [http://10.0.0.1:8000, http://10.0.0.2:8000]
    api_key_file: /etc/llmflow/vllm.key
    cost: {input_per_mtok: 0.01, output_per_mtok: 0.05, cached_per_mtok: 0.001}
  - name: llama-batch
    upstream_model: llama
    pool: b300
    class: batch
    upstreams: [http://10.0.0.3:8000]
    api_key_file: /etc/llmflow/vllm.key
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen || c.FairShare.HalfLife != 7*24*time.Hour || !c.FailClosed() {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.ModelGroups[0].UpstreamModel != "llama" || c.ModelGroups[0].Class != Interactive {
		t.Fatalf("group defaults: %+v", c.ModelGroups[0])
	}
	if c.ModelGroups[1].Class != Batch {
		t.Fatalf("class: %v", c.ModelGroups[1].Class)
	}
	if c.TeamShare("etabA") != 0.8125 || c.TeamShare("nobody") != 0 {
		t.Fatal("TeamShare")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":   strings.Replace(valid, "teams:", "temas: []\nteams:", 1),
		"shares > 1":      strings.Replace(valid, "0.1875", "0.5", 1),
		"bad scheme":      strings.Replace(valid, "http://10.0.0.3:8000", "file:///etc/passwd", 1),
		"userinfo":        strings.Replace(valid, "http://10.0.0.3:8000", "http://u:p@10.0.0.3:8000", 1),
		"query":           strings.Replace(valid, "http://10.0.0.3:8000", "http://10.0.0.3:8000?x=1", 1),
		"undeclared pool": strings.Replace(valid, "pool: b300\n    class", "pool: nope\n    class", 1),
		"bad class":       strings.Replace(valid, "class: batch", "class: urgent", 1),
		"no master":       strings.Replace(valid, "master_key_file: /etc/llmflow/master.key", "", 1),
		"bad team name":   strings.Replace(valid, "etabB", "etab B", 1),
		"pg without dsn":  valid + "store: {kind: postgres}\n",
		"zero capacity":   strings.Replace(valid, "capacity: 256", "capacity: 0", 1),
		"negative cost":   strings.Replace(valid, "input_per_mtok: 0.01", "input_per_mtok: -1", 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestReadSecret(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	if err := os.WriteFile(p, []byte("  sk-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSecret(p)
	if err != nil || s != "sk-secret-value" {
		t.Fatalf("%q %v", s, err)
	}
	if err := os.WriteFile(p, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(p); err == nil || strings.Contains(err.Error(), "a") && strings.Contains(err.Error(), "\nb") {
		t.Fatalf("multi-line secret: %v", err)
	}
	if err := os.WriteFile(p, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(p); err == nil {
		t.Fatal("empty secret accepted")
	}
	if _, err := ReadSecret(dir); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := ReadSecret(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing accepted")
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p + ".missing"); err == nil {
		t.Fatal("missing file accepted")
	}
}
