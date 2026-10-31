# Two institutions sharing Claude on Amazon Bedrock in an EU region, each
# with the credentials of an AWS account of its own (so their prompt caches
# stay apart), under hard budgets. Parsed by the test suite
# (config/examples_test.go) and by check-config.
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
pool "bedrock" { capacity = 32 } # concurrent requests the institutions share at Bedrock

model "llama-3.3-70b" {
  upstream_model = "meta-llama/Llama-3.3-70B-Instruct"
  pool           = "b300"
  key_file       = "/etc/llmflow/vllm.key"
  upstream "gpu01" { base_url = "http://10.0.0.11:8000" }
}

model "claude-sonnet-bedrock" {
  provider = "bedrock"
  # The endpoint is derived: https://bedrock-runtime.eu-west-3.amazonaws.com
  region = "eu-west-3"
  # The exact model ID or inference profile ID: the price below is the price of
  # THIS ID. An EU profile keeps inference in the EU regions it lists.
  model_id        = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
  pool            = "bedrock"
  allow_external  = true
  orgs            = ["etabA", "etabB"]
  anthropic_betas = ["context-management-2025-06-27"]
  # Hard budgets: input bounded by the context window, output by max_tokens.
  context_window    = 200000
  max_output_tokens = 64000
  cost {
    # USD per million tokens, from aws.amazon.com/bedrock/pricing
    unit                    = "USD"
    price_date              = "2026-10-31"
    input_per_mtok          = 3
    output_per_mtok         = 15
    cached_per_mtok         = 0.3
    cache_write_5m_per_mtok = 3.75
    cache_write_1h_per_mtok = 6
  }
  # Secret references: a file or a Vault reference, never an environment
  # variable or the command line.
  org_credentials "etabA" {
    access_key_id_file     = "vault:secret/llmflow/bedrock-etabA#access_key_id"
    secret_access_key_file = "vault:secret/llmflow/bedrock-etabA#secret_access_key"
    session_token_file     = "vault:secret/llmflow/bedrock-etabA#session_token"
  }
  org_credentials "etabB" {
    bearer_token_file = "/etc/llmflow/bedrock-etabB.apikey" # a short-term Bedrock API key
  }
}

# Where the vault: references above are read (OpenBao / Vault KV).
vault {
  address    = "https://bao.example.org:8200"
  token_file = "/etc/llmflow/vault.token"
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
