package config

import (
	"strings"
	"testing"
	"time"
)

const guardModel = `
model "lg" {
  upstream_model = "meta-llama/Llama-Guard-3-8B"
  pool           = "b300"
  class          = "guard"
  key_file       = "/etc/llmflow/vllm.key"
  upstream "g" { base_url = "http://10.0.0.9:8000" }
}
`

func TestParseGuardrails(t *testing.T) {
	c, err := parse(valid + guardModel + `
guardrail "secrets" {
  type  = "hide_secrets"
  orgs  = ["etabA"]
}
guardrail "pii" {
  type     = "pii"
  teams    = ["etabB"]
  actions  = { iban = "block", phone = "log" }
  on_error = "allow"
  timeout  = "500ms"
}
guardrail "lg" {
  type               = "llama_guard"
  model              = "lg"
  actions            = { S10 = "log" }
  stream_check_bytes = 2048
}
guardrail "pg" {
  type      = "prompt_guard"
  model     = "lg"
  threshold = 0.8
}
guardrail "ext" {
  type           = "http"
  url            = "https://guard.example.org/check"
  token_file     = "/etc/llmflow/guard.token"
  allow_external = true
}
`)
	if err != nil {
		t.Fatal(err)
	}
	g := c.Guardrails
	if len(g) != 5 {
		t.Fatalf("%+v", g)
	}
	if s := g[0]; s.Action != ActionMask || !s.Pre || !s.Post || s.FailOpen || s.Timeout != 2*time.Second || s.Orgs[0] != "etabA" {
		t.Errorf("secrets defaults: %+v", s)
	}
	if p := g[1]; !p.FailOpen || p.Timeout != 500*time.Millisecond || p.Actions["iban"] != ActionBlock {
		t.Errorf("pii: %+v", p)
	}
	if l := g[2]; l.Action != ActionBlock || l.StreamCheckBytes != 2048 || l.Model != "lg" {
		t.Errorf("llama_guard: %+v", l)
	}
	if p := g[3]; !p.Pre || p.Post || p.Threshold != 0.8 {
		t.Errorf("prompt_guard: pre only by default: %+v", p)
	}
	if !strings.Contains(strings.Join(c.SecretRefs(), ","), "/etc/llmflow/guard.token") {
		t.Error("token_file is not a secret reference")
	}
	if c.ModelGroups[2].Class != Guard {
		t.Errorf("class %q", c.ModelGroups[2].Class)
	}
}

func TestParseAudioLimits(t *testing.T) {
	c, err := parse(strings.Replace(valid, "cached_per_mtok = 0.001", "cached_per_mtok = 0.001\n    audio_per_hour = 0.5", 1) + "limits { max_audio_bytes = 1048576 }\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelGroups[0].Cost.AudioPerHour != 0.5 || c.Limits.MaxAudioBytes != 1<<20 {
		t.Fatalf("%+v %+v", c.ModelGroups[0].Cost, c.Limits)
	}
	if d, _ := parse(valid); d.Limits.MaxAudioBytes != DefaultMaxAudioBytes {
		t.Fatalf("default %d", d.Limits.MaxAudioBytes)
	}
	for _, bad := range []string{"limits { max_audio_bytes = 0 }\n", "limits { max_audio_bytes = 2147483648 }\n"} {
		if _, err := parse(valid + bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := parse(strings.Replace(valid, "cached_per_mtok = 0.001", "cached_per_mtok = 0.001\n    audio_per_hour = -1", 1)); err == nil {
		t.Error("negative audio price accepted")
	}
}

func TestParseGuardrailsRejects(t *testing.T) {
	cases := map[string]string{
		"type":            `guardrail "a" { type = "magic" }`,
		"stage":           `guardrail "a" { type = "pii", stages = ["during"] }`,
		"prompt post":     `guardrail "a" { type = "prompt_guard", model = "lg", stages = ["post"] }`,
		"unknown team":    `guardrail "a" { type = "pii", teams = ["etbaA"] }`,
		"unknown org":     `guardrail "a" { type = "pii", orgs = ["etbaA"] }`,
		"on_error":        `guardrail "a" { type = "pii", on_error = "maybe" }`,
		"timeout":         `guardrail "a" { type = "pii", timeout = "2m" }`,
		"action":          `guardrail "a" { type = "llama_guard", model = "lg", action = "mask" }`,
		"pii kind":        `guardrail "a" { type = "pii", actions = { ssn = "block" } }`,
		"lg category":     `guardrail "a" { type = "llama_guard", model = "lg", actions = { violence = "log" } }`,
		"unknown model":   `guardrail "a" { type = "llama_guard", model = "nope" }`,
		"not guard class": `guardrail "a" { type = "llama_guard", model = "llama" }`,
		"model on pii":    `guardrail "a" { type = "pii", model = "lg" }`,
		"threshold":       `guardrail "a" { type = "prompt_guard", model = "lg", threshold = 1.5 }`,
		"http external":   `guardrail "a" { type = "http", url = "https://x.example/c" }`,
		"http plain":      `guardrail "a" { type = "http", url = "http://x.example/c", allow_external = true }`,
		"url on pii":      `guardrail "a" { type = "pii", url = "https://x.example/c" }`,
		"duplicate": `guardrail "a" { type = "pii" }
guardrail "a" { type = "pii" }`,
		"stream bytes": `guardrail "a" { type = "llama_guard", model = "lg", stream_check_bytes = 8 }`,
	}
	format := func(block string) string {
		block = strings.ReplaceAll(block, "{ type", "{\n  type")
		block = strings.ReplaceAll(block, ", ", "\n  ")
		block = strings.ReplaceAll(block, "] }", "]\n}")
		block = strings.ReplaceAll(block, `" }`, "\"\n}")
		return strings.ReplaceAll(block, " }", "\n}")
	}
	// The control: the same formatting of a valid block parses, so the
	// cases below fail on their content, not on their syntax.
	if _, err := parse(valid + guardModel + format(`guardrail "a" { type = "llama_guard", model = "lg", threshold_unused = 1 }`) + "\n"); err == nil ||
		!strings.Contains(err.Error(), "threshold_unused") {
		t.Fatalf("control: %v", err)
	}
	if _, err := parse(valid + guardModel + format(`guardrail "a" { type = "pii", actions = { iban = "block" } }`) + "\n"); err != nil {
		t.Fatalf("control: %v", err)
	}
	for name, block := range cases {
		block = format(block)
		_, err := parse(valid + guardModel + block + "\n")
		if err == nil {
			t.Errorf("%s: accepted:\n%s", name, block)
			continue
		}
		if msg := err.Error(); strings.Contains(msg, "Missing newline") || strings.Contains(msg, "Argument or block definition required") ||
			strings.Contains(msg, "Unsupported argument") || strings.Contains(msg, "Invalid single-argument block") {
			t.Errorf("%s: a syntax error, not the check: %v", name, err)
		}
	}
}
