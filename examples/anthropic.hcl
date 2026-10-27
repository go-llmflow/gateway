# Two institutions sharing one Anthropic organisation's API usage, each in
# a workspace of its own (one API key per institution), with a local vLLM
# model to fall back on. Parsed by the test suite (config/examples_test.go).
master_key_file     = "/etc/llmflow/master.key"
cache_salt_key_file = "/etc/llmflow/cache-salt.key"

org "etabA" {
  share = 0.8125
  team "lab-nlp" {}
}
org "etabB" {
  share = 0.1875
  team "lab-chem" {}
}

pool "b300" { capacity = 256 }
pool "anthropic" { capacity = 32 } # concurrent requests the institutions share at Anthropic

model "llama-3.3-70b" {
  upstream_model = "meta-llama/Llama-3.3-70B-Instruct"
  pool           = "b300"
  key_file       = "/etc/llmflow/vllm.key"
  upstream "gpu01" { base_url = "http://10.0.0.11:8000" }
}

model "claude-sonnet" {
  provider        = "anthropic"
  upstream_model  = "claude-sonnet-5-5"
  pool            = "anthropic"
  allow_external  = true
  orgs            = ["etabA", "etabB"]
  anthropic_betas = ["context-management-2025-06-27"]
  # Under a budget, a request is held at its worst-case cost before it is
  # sent (hard budgets): its input is bounded by the context window (the
  # largest any allowed beta can reach) and its output by max_tokens.
  context_window    = 200000
  max_output_tokens = 64000
  cost {
    # USD per million tokens, from platform.claude.com/docs/en/about-claude/pricing
    unit                    = "USD"
    price_date              = "2026-10-26"
    input_per_mtok          = 2
    output_per_mtok         = 10
    cached_per_mtok         = 0.1
    cache_write_5m_per_mtok = 2.5
    cache_write_1h_per_mtok = 4
  }
  upstream "api" { base_url = "https://api.anthropic.com" }
  org_key "etabA" { key_file = "/etc/llmflow/anthropic-etabA.key" }
  org_key "etabB" { key_file = "/etc/llmflow/anthropic-etabB.key" }
  fallbacks   = ["llama-3.3-70b"]
  fallback_on = ["unavailable"]
}

# Required before prompts leave the site (external_requires_guardrails).
guardrail "pii" {
  type = "pii"
  orgs = ["etabA", "etabB"]
}
guardrail "secrets" {
  type = "hide_secrets"
  orgs = ["etabA", "etabB"]
}
