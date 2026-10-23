package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
master_key_file = "/etc/llmflow/master.key"

team "etabA" { share = 0.8125 }
team "etabB" { share = 0.1875 }

pool "b300" { capacity = 256 }

model "llama" {
  pool     = "b300"
  key_file = "/etc/llmflow/vllm.key"
  cost {
    input_per_mtok  = 0.01
    output_per_mtok = 0.05
    cached_per_mtok = 0.001
  }
  upstream "gpu01" { base_url = "http://10.0.0.1:8000" }
  upstream "gpu02" {
    base_url = "http://10.0.0.2:8000"
    key_file = "/etc/llmflow/gpu02.key"
  }
}

model "llama-batch" {
  upstream_model = "llama"
  pool           = "b300"
  class          = "batch"
  upstream "gpu03" {
    base_url = "http://10.0.0.3:8000"
    key_file = "/etc/llmflow/vllm.key"
  }
}
`

func parse(src string) (*Config, error) { return Parse([]byte(src), "test.hcl") }

func TestParseValid(t *testing.T) {
	c, err := parse(valid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen || c.FairShare.HalfLife != 7*24*time.Hour || !c.FailClosed() || c.Retries != 2 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	g := c.ModelGroups[0]
	if g.UpstreamModel != "llama" || g.Class != Interactive || g.Cost.OutputPerMTok != 0.05 {
		t.Fatalf("group: %+v", g)
	}
	if g.Upstreams[0].KeyFile != "/etc/llmflow/vllm.key" || g.Upstreams[1].KeyFile != "/etc/llmflow/gpu02.key" {
		t.Fatalf("key file inheritance: %+v", g.Upstreams)
	}
	if c.ModelGroups[1].Class != Batch {
		t.Fatalf("class: %v", c.ModelGroups[1].Class)
	}
	if c.TeamShare("etabA") != 0.8125 || c.TeamShare("nobody") != 0 {
		t.Fatal("TeamShare")
	}
	if kf := c.KeyFiles(); len(kf) != 2 {
		t.Fatalf("key files %v", kf)
	}
}

func TestParseOptionalBlocks(t *testing.T) {
	c, err := parse(valid + `
listen = "0.0.0.0:9000"
retries = 0
budget_refresh = "5s"
key_cache_ttl = "1m"
shutdown_grace = "3s"
expose_overhead_header = true
metrics_token_file = "/m"
store {
  kind = "postgres"
  dsn_file = "/dsn"
  fail_closed = false
  queue_size = 10
  batch_size = 5
}
limits {
  max_body_bytes = 1024
  max_header_bytes = 2048
  read_header_timeout = "1s"
  read_timeout = "2s"
  idle_timeout = "3s"
  upstream_header_timeout = "4s"
  max_request_duration = "5s"
  max_response_bytes = 4096
}
fairshare { half_life = "1h" }
health {
  interval = "1s"
  timeout = "2s"
  cooldown = "3s"
  path = "/ping"
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Retries != 0 || c.FailClosed() || c.Store.Kind != "postgres" || c.Limits.MaxRequestDuration != 5*time.Second ||
		c.Health.Path != "/ping" || c.FairShare.HalfLife != time.Hour || !c.ExposeOverheadHdr || c.MetricsTokenFile != "/m" ||
		c.Limits.MaxResponseBytes != 4096 || c.Store.BatchSize != 5 || c.KeyCacheTTL != time.Minute {
		t.Fatalf("%+v", c)
	}
}

// Every rejection carries the file:line:column of the offending value.
func TestParseRejectsWithPosition(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"unknown attribute", strings.Replace(valid, `master_key_file`, "temas = 1\nmaster_key_file", 1), `test.hcl:2:1: Unsupported argument`},
		{"shares > 1", strings.Replace(valid, "0.1875", "0.5", 1), `test.hcl:5:24: Shares exceed the pool`},
		{"bad scheme", strings.Replace(valid, "http://10.0.0.3:8000", "file:///etc/passwd", 1), `test.hcl:29:16: Invalid base_url`},
		{"userinfo", strings.Replace(valid, "http://10.0.0.3:8000", "http://u:p@10.0.0.3:8000", 1), "Invalid base_url"},
		{"query", strings.Replace(valid, "http://10.0.0.3:8000", "http://10.0.0.3:8000?x=1", 1), "Invalid base_url"},
		{"undeclared pool", strings.Replace(valid, `pool           = "b300"`, `pool           = "nope"`, 1), `test.hcl:26:20: Unknown pool`},
		{"bad class", strings.Replace(valid, `"batch"`, `"urgent"`, 1), "Invalid class"},
		{"no master", strings.Replace(valid, `master_key_file = "/etc/llmflow/master.key"`, "", 1), "Missing required argument"},
		{"bad team name", strings.Replace(valid, `"etabB"`, `"etab B"`, 1), `test.hcl:5:6: Invalid team name`},
		{"duplicate team", valid + `team "etabA" { share = 0 }`, "Duplicate team"},
		{"pg without dsn", valid + `store { kind = "postgres" }`, "Missing DSN file"},
		{"bad store kind", valid + `store { kind = "redis" }`, "Invalid store kind"},
		{"zero capacity", strings.Replace(valid, "capacity = 256", "capacity = 0", 1), `test.hcl:7:26: Invalid capacity`},
		{"negative cost", strings.Replace(valid, "input_per_mtok  = 0.01", "input_per_mtok  = -1", 1), "Invalid cost"},
		{"bad duration", valid + `fairshare { half_life = "a week" }`, "Invalid duration"},
		{"negative duration", valid + `health { interval = "-1s" }`, "Invalid duration"},
		{"bad health path", valid + `health { path = "health?x" }`, "Invalid health path"},
		{"no key file", strings.Replace(valid, "  key_file = \"/etc/llmflow/vllm.key\"\n  cost", "  cost", 1), "Missing key file"},
		{"no upstream", valid + `model "x" { pool = "b300" }`, "No upstream"},
		{"duplicate upstream", strings.Replace(valid, `"gpu02"`, `"gpu01"`, 1), "Duplicate upstream"},
		{"no model", "master_key_file = \"/m\"\n", "No model"},
		{"retries", valid + "retries = 11\n", "Invalid retries"},
		{"body limit", valid + "limits { max_body_bytes = 0 }\n", "Invalid body limit"},
		{"syntax", valid + "team {", "test.hcl:"},
		{"string for number", strings.Replace(valid, "capacity = 256", `capacity = "many"`, 1), "Unsuitable value type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.src)
			if err == nil {
				t.Fatal("accepted")
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("not a *config.Error: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// HCL quoted strings are templates. The evaluation context is empty, so
// interpolations of variables and function calls are refused, and the
// escaped forms produce the literal text.
func TestTemplatesAndStaticContext(t *testing.T) {
	refused := map[string]string{
		"variable in a URL":       strings.Replace(valid, "http://10.0.0.3:8000", "http://${host}:8000", 1),
		"environment":             strings.Replace(valid, `"/etc/llmflow/master.key"`, `"${env.HOME}/master.key"`, 1),
		"function call":           strings.Replace(valid, `"/etc/llmflow/master.key"`, `file("/etc/shadow")`, 1),
		"bare variable":           strings.Replace(valid, `capacity = 256`, `capacity = var.c`, 1),
		"directive with variable": strings.Replace(valid, `upstream_model = "llama"`, `upstream_model = "%{ if x }a%{ endif }"`, 1),
	}
	for name, src := range refused {
		t.Run(name, func(t *testing.T) {
			_, err := parse(src)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), "not allowed") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	// Escapes give the literal characters.
	c, err := parse(strings.Replace(valid, `upstream_model = "llama"`, `upstream_model = "org/m$${x}%%{y}"`, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ModelGroups[1].UpstreamModel; got != "org/m${x}%{y}" {
		t.Fatalf("escaped template gave %q", got)
	}
	// A lone "$" or "%" needs no escape.
	c, err = parse(strings.Replace(valid, `upstream_model = "llama"`, `upstream_model = "50%-$cheap"`, 1))
	if err != nil || c.ModelGroups[1].UpstreamModel != "50%-$cheap" {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestErrorFormat(t *testing.T) {
	_, err := parse("master_key_file = \"/m\"\nteam \"a\" { share = \"x\" }\npool \"p\" { capacity = \"y\" }\n")
	if err == nil || !strings.HasPrefix(err.Error(), "test.hcl:") {
		t.Fatalf("%v", err)
	}
	if strings.Count(err.Error(), "\n") < 1 {
		t.Fatalf("expected several diagnostics: %v", err)
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
	if err := os.WriteFile(p, []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(p); err == nil || strings.Contains(err.Error(), "second") {
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

// The example in the README must stay valid.
func TestExampleConfig(t *testing.T) {
	c, err := Load(filepath.Join("..", "examples", "gateway.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ModelGroups) != 2 || c.Pools[0].Capacity != 256 || c.TeamShare("etabB") != 0.1875 {
		t.Fatalf("%+v", c)
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.hcl")
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

func TestValidateUpstream(t *testing.T) {
	for _, u := range []string{"http://h:1", "https://h/v"} {
		if err := ValidateUpstream(u); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"%", "ftp://h", "http://", "http://h?", "http://h#f"} {
		if ValidateUpstream(u) == nil {
			t.Fatalf("%s accepted", u)
		}
	}
}

// FuzzParse: the configuration is untrusted input. Parsing must not panic,
// and whatever it accepts must satisfy the invariants the gateway relies on.
func FuzzParse(f *testing.F) {
	f.Add([]byte(valid))
	f.Add([]byte(valid + `store { kind = "postgres" dsn_file = "/d" }`))
	f.Add([]byte("master_key_file = \"$${x}\"\nmodel \"m\" {\n upstream \"u\" {\n  base_url = \"http://h\"\n  key_file = <<EOT\n/k\nEOT\n }\n}\npool \"m\" { capacity = 1 }\n"))
	f.Fuzz(func(t *testing.T, src []byte) {
		c, err := Parse(src, "fuzz.hcl")
		if err != nil {
			return
		}
		sum := 0.0
		for _, tm := range c.Teams {
			if !ValidName(tm.Name) || tm.Share < 0 || tm.Share > 1 {
				t.Fatalf("bad team %+v", tm)
			}
			sum += tm.Share
		}
		if sum > 1+1e-9 {
			t.Fatalf("shares sum %v", sum)
		}
		pools := map[string]bool{}
		for _, p := range c.Pools {
			if p.Capacity <= 0 {
				t.Fatalf("capacity %d", p.Capacity)
			}
			pools[p.Name] = true
		}
		if len(c.ModelGroups) == 0 || c.MasterKeyFile == "" {
			t.Fatal("accepted without model or master key")
		}
		for _, g := range c.ModelGroups {
			if !pools[g.Pool] || len(g.Upstreams) == 0 {
				t.Fatalf("group %+v", g)
			}
			for _, u := range g.Upstreams {
				if ValidateUpstream(u.BaseURL) != nil || u.KeyFile == "" {
					t.Fatalf("upstream %+v", u)
				}
			}
		}
	})
}
